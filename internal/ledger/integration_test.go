package ledger

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"bank-ledger/migrations"

	"github.com/jackc/pgx/v5/pgxpool"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; PostgreSQL integration test skipped")
	}
	ctx := context.Background()
	admin, e := pgxpool.New(ctx, url)
	if e != nil {
		t.Fatal(e)
	}
	schema := fmt.Sprintf("ledger_test_%d", time.Now().UnixNano())
	if _, e = admin.Exec(ctx, `CREATE SCHEMA `+schema); e != nil {
		admin.Close()
		t.Fatal(e)
	}
	cfg, e := pgxpool.ParseConfig(url)
	if e != nil {
		t.Fatal(e)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.MaxConns = 24
	db, e := pgxpool.NewWithConfig(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		db.Close()
		_, e := admin.Exec(ctx, `DROP SCHEMA `+schema+` CASCADE`)
		admin.Close()
		if e != nil {
			t.Error(e)
		}
	})
	if e = migrations.Apply(ctx, db); e != nil {
		t.Fatal(e)
	}
	if e = migrations.Apply(ctx, db); e != nil {
		t.Fatal(e)
	}
	return &Store{DB: db}
}
func setupAccounts(t *testing.T, s *Store) (Account, Account, Account) {
	t.Helper()
	ctx := context.Background()
	create := func(code, side string, overdraft bool) Account {
		a, e := s.CreateAccount(ctx, Account{Code: code, Name: code, Kind: "asset", Currency: "USD", NormalSide: side, AllowOverdraft: overdraft})
		if e != nil {
			t.Fatal(e)
		}
		return a
	}
	a, b, funding := create("alice", "debit", false), create("bob", "debit", false), create("funding", "credit", true)
	_, e := s.Post(ctx, Posting{Key: "fund", Entries: []Entry{{a.ID, "debit", 100}, {funding.ID, "credit", 100}}})
	if e != nil {
		t.Fatal(e)
	}
	return a, b, funding
}
func TestPostgresPosting(t *testing.T) {
	s := testStore(t)
	a, b, _ := setupAccounts(t, s)
	ctx := context.Background()
	tr := Transfer{Key: "transfer", From: a.ID, To: b.ID, Amount: 30}
	first, e := s.Transfer(ctx, tr)
	if e != nil {
		t.Fatal(e)
	}
	replay, e := s.Transfer(ctx, tr)
	if e != nil || !replay.Replayed || replay.ID != first.ID {
		t.Fatalf("replay: %+v %v", replay, e)
	}
	tr.Amount = 31
	if _, e = s.Transfer(ctx, tr); !errors.Is(e, ErrConflict) {
		t.Fatalf("conflict: %v", e)
	}
	tr.Key = "too-much"
	tr.Amount = 71
	if _, e = s.Transfer(ctx, tr); e == nil {
		t.Fatal("overdraft accepted")
	}
	tr.Amount = 70
	if _, e = s.Transfer(ctx, tr); e != nil {
		t.Fatalf("failed key must be reusable: %v", e)
	}
	aa, _ := s.Account(ctx, a.ID)
	bb, _ := s.Account(ctx, b.ID)
	if aa.Balance != 0 || bb.Balance != 100 {
		t.Fatalf("balances: %d %d", aa.Balance, bb.Balance)
	}
	var count int
	if e = s.DB.QueryRow(ctx, `SELECT count(*) FROM journals`).Scan(&count); e != nil || count != 3 {
		t.Fatalf("journal count %d %v", count, e)
	}
	var mismatch int
	if e = s.DB.QueryRow(ctx, `SELECT count(*) FROM accounts a WHERE balance::numeric <> (SELECT coalesce(sum(CASE WHEN e.side=a.normal_side THEN amount::numeric ELSE -amount::numeric END),0) FROM entries e WHERE e.account_id=a.id)`).Scan(&mismatch); e != nil || mismatch != 0 {
		t.Fatalf("balance reconciliation: %d %v", mismatch, e)
	}
}
func TestPostgresConcurrency(t *testing.T) {
	s := testStore(t)
	a, b, _ := setupAccounts(t, s)
	ctx := context.Background()
	var wg sync.WaitGroup
	var mu sync.Mutex
	success := 0
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, e := s.Transfer(ctx, Transfer{Key: fmt.Sprintf("spend-%d", i), From: a.ID, To: b.ID, Amount: 10})
			if e == nil {
				mu.Lock()
				success++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	aa, _ := s.Account(ctx, a.ID)
	bb, _ := s.Account(ctx, b.ID)
	if success != 10 || aa.Balance != 0 || bb.Balance != 100 {
		t.Fatalf("success=%d balances=%d,%d", success, aa.Balance, bb.Balance)
	}
	// Concurrent retries of the same request must produce one journal.
	ids := make(chan int64, 20)
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, e := s.Transfer(ctx, Transfer{Key: "shared", From: b.ID, To: a.ID, Amount: 1})
			ids <- r.ID
			errs <- e
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	var id int64
	for n := range ids {
		if id != 0 && id != n {
			t.Fatal("duplicate posting")
		}
		id = n
	}
	aa, _ = s.Account(ctx, a.ID)
	if aa.Balance != 1 {
		t.Fatal(aa.Balance)
	}
}
func TestPostgresDatabaseGuards(t *testing.T) {
	s := testStore(t)
	a, b, _ := setupAccounts(t, s)
	ctx := context.Background()
	// Bypass the application: deferred constraints must reject and roll back.
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	var id int64
	if e = tx.QueryRow(ctx, `INSERT INTO journals(idempotency_key,request_hash,description) VALUES('bad','x','') RETURNING id`).Scan(&id); e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(ctx, `INSERT INTO entries VALUES($1,1,$2,'debit',10)`, id, a.ID); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(ctx); e == nil {
		t.Fatal("unbalanced journal committed")
	}
	aa, _ := s.Account(ctx, a.ID)
	if aa.Balance != 100 {
		t.Fatal("rollback failed")
	}
	statements := []string{
		`UPDATE entries SET amount=1`, `DELETE FROM entries`, `UPDATE journals SET description='edited'`,
		`UPDATE accounts SET balance=999`, `UPDATE accounts SET currency='EUR'`,
		`INSERT INTO entries SELECT id,99,` + fmt.Sprint(b.ID) + `,'debit',1 FROM journals LIMIT 1`,
		`INSERT INTO accounts(code,name,kind,currency,normal_side,balance) VALUES('illegal','illegal','asset','USD','debit',50)`,
		`INSERT INTO journals(idempotency_key,request_hash,description) VALUES('empty','x','')`,
	}
	for _, q := range statements {
		if _, e = s.DB.Exec(ctx, q); e == nil {
			t.Errorf("accepted: %s", q)
		}
	}
	foreign, e := s.CreateAccount(ctx, Account{Code: "eur", Name: "Euro", Kind: "asset", Currency: "EUR", NormalSide: "debit"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Post(ctx, Posting{Key: "fx", Entries: []Entry{{foreign.ID, "debit", 1}, {a.ID, "credit", 1}}}); !errors.Is(e, ErrInvalid) {
		t.Fatalf("mixed currency: %v", e)
	}
}

func TestPostgresConflictAndEarlyConstraintChecks(t *testing.T) {
	s := testStore(t)
	a, b, _ := setupAccounts(t, s)
	ctx := context.Background()
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	var id int64
	if e = tx.QueryRow(ctx, `INSERT INTO journals(idempotency_key,request_hash,description) VALUES('conflict','x','') RETURNING id`).Scan(&id); e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(ctx, `INSERT INTO entries VALUES($1,1,$2,'debit',10),($1,2,$3,'credit',10)`, id, b.ID, a.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(ctx, `INSERT INTO entries VALUES($1,1,$2,'debit',500) ON CONFLICT DO NOTHING`, id, b.ID); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	bb, e := s.Account(ctx, b.ID)
	if e != nil || bb.Balance != 10 {
		t.Fatalf("skipped entry changed balance: %+v %v", bb, e)
	}
	tx, e = s.DB.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	if e = tx.QueryRow(ctx, `INSERT INTO journals(idempotency_key,request_hash,description) VALUES('early','x','') RETURNING id`).Scan(&id); e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(ctx, `INSERT INTO entries VALUES($1,1,$2,'debit',1),($1,2,$3,'credit',1)`, id, b.ID, a.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(ctx, `SET CONSTRAINTS ALL IMMEDIATE`); e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(ctx, `SET CONSTRAINTS ALL DEFERRED`); e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(ctx, `INSERT INTO entries VALUES($1,3,$2,'debit',1)`, id, b.ID); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(ctx); e == nil {
		t.Fatal("early check allowed an unbalanced commit")
	}
}

func TestPostgresOverflowAndJournalReplay(t *testing.T) {
	s := testStore(t)
	a, b, f := setupAccounts(t, s)
	ctx := context.Background()
	p := Posting{Key: "max", Entries: []Entry{{f.ID, "credit", 9223372036854775807}, {b.ID, "debit", 9223372036854775807}}}
	// Funding would overflow; changes to the other account must roll back too.
	if _, e := s.Post(ctx, p); e == nil {
		t.Fatal("overflow accepted")
	}
	bb, e := s.Account(ctx, b.ID)
	if e != nil || bb.Balance != 0 {
		t.Fatalf("overflow rollback: %+v %v", bb, e)
	}
	p.Key = "reorder"
	p.Entries = []Entry{{a.ID, "credit", 5}, {b.ID, "debit", 5}}
	first, e := s.Post(ctx, p)
	if e != nil {
		t.Fatal(e)
	}
	if p.Entries[0].Side != "credit" {
		t.Fatal("posting mutated caller entries")
	}
	second, e := s.Post(ctx, p)
	if e != nil || !second.Replayed || first.ID != second.ID {
		t.Fatalf("journal retry: %+v %v", second, e)
	}
	j, e := s.Journal(ctx, first.ID)
	if e != nil || len(j.Entries) != 2 {
		t.Fatalf("journal read: %+v %v", j, e)
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()
	if _, e = s.Transfer(cancelCtx, Transfer{Key: "cancel", From: a.ID, To: b.ID, Amount: 1}); e == nil {
		t.Fatal("cancelled request succeeded")
	}
	if _, e = s.Transfer(ctx, Transfer{Key: "cancel", From: a.ID, To: b.ID, Amount: 1}); e != nil {
		t.Fatal(e)
	}
}

func TestPostgresOpposingTransfers(t *testing.T) {
	s := testStore(t)
	a, b, _ := setupAccounts(t, s)
	ctx := context.Background()
	if _, e := s.Transfer(ctx, Transfer{Key: "split", From: a.ID, To: b.ID, Amount: 50}); e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			from, to := a.ID, b.ID
			if i%2 == 0 {
				from, to = to, from
			}
			_, e := s.Transfer(ctx, Transfer{Key: fmt.Sprint("opposite-", i), From: from, To: to, Amount: 1})
			errs <- e
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	aa, e := s.Account(ctx, a.ID)
	if e != nil || aa.Balance != 50 {
		t.Fatalf("opposing transfers: %+v %v", aa, e)
	}
}
