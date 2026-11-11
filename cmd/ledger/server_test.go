package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"bank-ledger/internal/ledger"
	"bank-ledger/migrations"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func serverDB(t *testing.T, c config) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, e := pgxpool.New(ctx, url)
	if e != nil {
		t.Fatal(e)
	}
	schema := fmt.Sprintf("ledger_server_%d", time.Now().UnixNano())
	if _, e = admin.Exec(ctx, `CREATE SCHEMA `+schema); e != nil {
		admin.Close()
		t.Fatal(e)
	}
	cfg, e := poolConfig(url, c)
	if e != nil {
		t.Fatal(e)
	}
	cfg.MaxConns = 3
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.ConnConfig.RuntimeParams["application_name"] = schema
	db, e := pgxpool.NewWithConfig(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		closePool(db, time.Second)
		_, e := admin.Exec(ctx, `DROP SCHEMA `+schema+` CASCADE`)
		admin.Close()
		if e != nil {
			t.Error(e)
		}
	})
	if e = migrations.Apply(ctx, db); e != nil {
		t.Fatal(e)
	}
	s := &ledger.Store{DB: db}
	for _, code := range []string{"a", "b"} {
		if _, e = s.CreateAccount(ctx, ledger.Account{Code: code, Name: code, Kind: "asset", Currency: "USD", NormalSide: "debit", AllowOverdraft: true}); e != nil {
			t.Fatal(e)
		}
	}
	return db
}
func startServer(t *testing.T, db *pgxpool.Pool, c config) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, listener, db, c) }()
	t.Cleanup(cancel)
	return "http://" + listener.Addr().String(), cancel, done
}
func waitAccountBlocked(t *testing.T, db *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		e := db.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity WHERE application_name=$1 AND state='active' AND wait_event_type='Lock' AND query LIKE 'SELECT id,code%'`, db.Config().ConnConfig.RuntimeParams["application_name"]).Scan(&n)
		if e != nil {
			t.Fatal(e)
		}
		if n > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("request did not block on account")
}
func waitPoolReleased(t *testing.T, db *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if db.Stat().AcquiredConns() == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("pool connection not released")
}

const transferBody = `{"idempotency_key":"blocked","from_account_id":1,"to_account_id":2,"amount":1}`

func TestPostgresLockTimeout(t *testing.T) {
	c := config{Request: 2 * time.Second, Lock: 100 * time.Millisecond, Statement: time.Second, IdleTransaction: 5 * time.Second, Shutdown: time.Second}
	db := serverDB(t, c)
	url, stop, done := startServer(t, db, c)
	tx, e := db.Begin(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(context.Background())
	if _, e = tx.Exec(context.Background(), `SELECT id FROM accounts WHERE id=1 FOR UPDATE`); e != nil {
		t.Fatal(e)
	}
	start := time.Now()
	response, e := http.Post(url+"/transfers", "application/json", strings.NewReader(transferBody))
	if e != nil {
		t.Fatal(e)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 503 || time.Since(start) > time.Second {
		t.Fatalf("lock timeout: %d %s", response.StatusCode, body)
	}
	_ = tx.Rollback(context.Background())
	waitPoolReleased(t, db)
	// The failed attempt must not reserve its key.
	response, e = http.Post(url+"/transfers", "application/json", strings.NewReader(transferBody))
	if e != nil {
		t.Fatal(e)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal(response.StatusCode)
	}
	stop()
	if e = <-done; e != nil {
		t.Fatal(e)
	}
}
func TestPostgresRequestDeadlineAndClientCancellation(t *testing.T) {
	for _, mode := range []string{"deadline", "client cancellation"} {
		t.Run(mode, func(t *testing.T) {
			c := config{Request: 250 * time.Millisecond, Lock: 5 * time.Second, Statement: 6 * time.Second, IdleTransaction: 5 * time.Second, Shutdown: time.Second}
			db := serverDB(t, c)
			url, stop, done := startServer(t, db, c)
			tx, e := db.Begin(context.Background())
			if e != nil {
				t.Fatal(e)
			}
			defer tx.Rollback(context.Background())
			if _, e = tx.Exec(context.Background(), `SELECT id FROM accounts WHERE id=1 FOR UPDATE`); e != nil {
				t.Fatal(e)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			request, e := http.NewRequestWithContext(ctx, "POST", url+"/transfers", strings.NewReader(transferBody))
			if e != nil {
				t.Fatal(e)
			}
			result := make(chan error, 1)
			go func() {
				response, e := http.DefaultClient.Do(request)
				if e == nil {
					defer response.Body.Close()
					_, _ = io.Copy(io.Discard, response.Body)
					if mode == "deadline" && response.StatusCode != 504 {
						e = fmt.Errorf("status %d", response.StatusCode)
					}
				}
				result <- e
			}()
			waitAccountBlocked(t, db)
			if mode == "client cancellation" {
				cancel()
			}
			select {
			case e = <-result:
				if mode == "deadline" && e != nil {
					t.Fatal(e)
				}
				if mode == "client cancellation" && e == nil {
					t.Fatal("client cancellation succeeded")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("request did not terminate")
			}
			_ = tx.Rollback(context.Background())
			waitPoolReleased(t, db)
			if _, e = (&ledger.Store{DB: db}).Transfer(context.Background(), ledger.Transfer{Key: "blocked", From: 1, To: 2, Amount: 1}); e != nil {
				t.Fatal(e)
			}
			stop()
			if e = <-done; e != nil {
				t.Fatal(e)
			}
		})
	}
}
func TestPostgresBoundedShutdown(t *testing.T) {
	c := config{Request: 10 * time.Second, Lock: 5 * time.Second, Statement: 6 * time.Second, IdleTransaction: 8 * time.Second, Shutdown: 400 * time.Millisecond}
	db := serverDB(t, c)
	url, stop, done := startServer(t, db, c)
	tx, e := db.Begin(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(context.Background())
	if _, e = tx.Exec(context.Background(), `SELECT id FROM accounts WHERE id=1 FOR UPDATE`); e != nil {
		t.Fatal(e)
	}
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		r, e := http.Post(url+"/transfers", "application/json", strings.NewReader(transferBody))
		if e == nil {
			r.Body.Close()
		}
	}()
	waitAccountBlocked(t, db)
	start := time.Now()
	stop()
	select {
	case e = <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(c.Shutdown + time.Second):
		t.Fatal("shutdown hung")
	}
	// Test fixture's lock is external to the server and must remain held while
	// the server cancels its own request and returns its checked-out connection.
	deadline := time.Now().Add(time.Second)
	for db.Stat().AcquiredConns() > 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if db.Stat().AcquiredConns() != 1 {
		t.Fatal("handler retained a pool connection")
	}
	_ = tx.Rollback(context.Background())
	closePool(db, c.Shutdown/2)
	if time.Since(start) > c.Shutdown+time.Second {
		t.Fatal("pool cleanup exceeded shutdown bound")
	}
	select {
	case <-clientDone:
	case <-time.After(time.Second):
		t.Fatal("client connection stayed open")
	}
}
func TestPoolCloseIsBounded(t *testing.T) {
	c := config{Request: time.Second, Lock: 100 * time.Millisecond, Statement: 500 * time.Millisecond, IdleTransaction: time.Second, Shutdown: time.Second}
	db := serverDB(t, c)
	conn, e := db.Acquire(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	start := time.Now()
	closePool(db, 30*time.Millisecond)
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("pool close hung")
	}
	conn.Release()
}

func TestPostgresStatementAndIdleTimeouts(t *testing.T) {
	c := config{Request: 2 * time.Second, Lock: 100 * time.Millisecond, Statement: 300 * time.Millisecond, IdleTransaction: 100 * time.Millisecond, Shutdown: time.Second}
	db := serverDB(t, c)
	start := time.Now()
	_, e := db.Exec(context.Background(), `SELECT pg_sleep(2)`)
	var pg *pgconn.PgError
	if !errors.As(e, &pg) || pg.Code != "57014" || time.Since(start) > time.Second {
		t.Fatalf("statement timeout: %v", e)
	}
	tx, e := db.Begin(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	time.Sleep(3 * c.IdleTransaction)
	_, e = tx.Exec(context.Background(), `SELECT 1`)
	if !errors.As(e, &pg) || pg.Code != "25P03" {
		t.Fatalf("idle transaction timeout: %v", e)
	}
	_ = tx.Rollback(context.Background())
	waitPoolReleased(t, db)
	if e = db.Ping(context.Background()); e != nil {
		t.Fatal(e)
	}
}

func TestPostgresGracefulCompletion(t *testing.T) {
	c := config{Request: 2 * time.Second, Lock: time.Second, Statement: 1500 * time.Millisecond, IdleTransaction: 3 * time.Second, Shutdown: time.Second}
	db := serverDB(t, c)
	url, stop, done := startServer(t, db, c)
	tx, e := db.Begin(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(context.Background())
	if _, e = tx.Exec(context.Background(), `SELECT id FROM accounts WHERE id=1 FOR UPDATE`); e != nil {
		t.Fatal(e)
	}
	result := make(chan error, 1)
	go func() {
		r, e := http.Post(url+"/transfers", "application/json", strings.NewReader(transferBody))
		if e == nil {
			r.Body.Close()
			if r.StatusCode != 200 {
				e = fmt.Errorf("status %d", r.StatusCode)
			}
		}
		result <- e
	}()
	waitAccountBlocked(t, db)
	stop()
	time.Sleep(20 * time.Millisecond)
	_ = tx.Rollback(context.Background())
	if e = <-result; e != nil {
		t.Fatal(e)
	}
	if e = <-done; e != nil {
		t.Fatal(e)
	}
}
