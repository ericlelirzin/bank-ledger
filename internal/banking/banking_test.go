package banking

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"bank-ledger/internal/ledger"
	"bank-ledger/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

func testStore(t *testing.T, version ...int) *Store {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("banking_test_%d", time.Now().UnixNano())
	if _, err = admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, err := admin.Exec(ctx, `DROP SCHEMA `+schema+` CASCADE`)
		admin.Close()
		if err != nil {
			t.Error(err)
		}
	})
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.MaxConns = 12
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if len(version) > 0 && version[0] == 3 {
		for _, name := range []string{"001_ledger.sql", "002_reliability.sql", "003_banking.sql"} {
			sql, e := os.ReadFile(filepath.Join("..", "..", "migrations", name))
			if e != nil {
				t.Fatal(e)
			}
			if _, e = db.Exec(ctx, string(sql)); e != nil {
				t.Fatal(e)
			}
		}
		if _, err = db.Exec(ctx, `CREATE TABLE schema_migrations(version integer PRIMARY KEY); INSERT INTO schema_migrations VALUES(1),(2),(3)`); err != nil {
			t.Fatal(err)
		}
		return &Store{DB: db}
	}
	for i := 0; i < 2; i++ {
		if err = migrations.Apply(ctx, db); err != nil {
			t.Fatal(err)
		}
	}
	return &Store{DB: db}
}
func funded(t *testing.T, s *Store) Account {
	t.Helper()
	a, err := s.Open(context.Background(), "Demo business")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Fund(context.Background(), a.ID, 10000, "initial-funding"); err != nil {
		t.Fatal(err)
	}
	return a
}
func balance(t *testing.T, s *Store, id, posted, available, reserved int64) {
	t.Helper()
	a, err := s.Account(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if a.Posted != posted || a.Available != available || a.Reserved != reserved {
		t.Fatalf("unexpected balances: %+v", a)
	}
	var bad int
	err = s.DB.QueryRow(context.Background(), `SELECT count(*) FROM accounts a WHERE a.balance <> coalesce((SELECT sum(CASE WHEN e.side=a.normal_side THEN e.amount::numeric ELSE -e.amount::numeric END) FROM entries e WHERE e.account_id=a.id),0)`).Scan(&bad)
	if err != nil || bad != 0 {
		t.Fatalf("ledger reconciliation: bad=%d err=%v", bad, err)
	}
}
func TestPostgresTransferLifecycle(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := funded(t, s)
	res, err := s.Fund(ctx, a.ID, 10000, "initial-funding")
	if err != nil || !res.Replayed {
		t.Fatalf("funding replay: %+v %v", res, err)
	}
	if _, err = s.Fund(ctx, a.ID, 999, "initial-funding"); !errors.Is(err, ledger.ErrConflict) {
		t.Fatalf("funding conflict: %v", err)
	}
	r := Request{Key: "supplier-1", AccountID: a.ID, Destination: "SIM-SUPPLIER-001", Amount: 1200}
	tr, err := s.CreateTransfer(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Status != "pending" || tr.ReserveJournalID == nil {
		t.Fatalf("pending transfer: %+v", tr)
	}
	balance(t, s, a.ID, 10000, 8800, 1200)
	again, err := s.CreateTransfer(ctx, r)
	if err != nil || again.ID != tr.ID {
		t.Fatalf("transfer replay: %+v %v", again, err)
	}
	r.Amount++
	if _, err = s.CreateTransfer(ctx, r); !errors.Is(err, ledger.ErrConflict) {
		t.Fatalf("conflict: %v", err)
	}
	ok, err := s.ProcessOne(ctx)
	if err != nil || !ok {
		t.Fatalf("process: %v %v", ok, err)
	}
	balance(t, s, a.ID, 8800, 8800, 0)
	tr, err = s.Transfer(ctx, tr.ID)
	if err != nil || tr.Status != "settled" || tr.SettlementJournalID == nil || tr.SettledAt == nil {
		t.Fatalf("settlement: %+v %v", tr, err)
	}
	if ok, err = s.ProcessOne(ctx); err != nil || ok {
		t.Fatalf("empty worker: %v %v", ok, err)
	}
	r.Amount--
	again, err = s.CreateTransfer(ctx, r)
	if err != nil || again.Status != "settled" {
		t.Fatalf("settled replay: %+v %v", again, err)
	}
}
func TestPostgresCompetingReservations(t *testing.T) {
	s := testStore(t)
	a := funded(t, s)
	ctx := context.Background()
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.CreateTransfer(ctx, Request{Key: fmt.Sprintf("race-%d", i), AccountID: a.ID, Destination: "SIM-001", Amount: 7000})
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("successful reservations = %d", success)
	}
	balance(t, s, a.ID, 10000, 3000, 7000)
	var n int
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM outgoing_transfers`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rollback of failed work: %d %v", n, err)
	}
}
func TestPostgresReceiptSurvivesSettlementRollback(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := funded(t, s)
	tr, err := s.CreateTransfer(ctx, Request{Key: "recover", AccountID: a.ID, Destination: "SIM-001", Amount: 1200})
	if err != nil {
		t.Fatal(err)
	}
	// Fail after the independently committed bank receipt and local journal insert.
	_, err = s.DB.Exec(ctx, `CREATE FUNCTION fail_settlement() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'test settlement failure'; END $$; CREATE TRIGGER test_failure BEFORE UPDATE ON outgoing_transfers FOR EACH ROW EXECUTE FUNCTION fail_settlement()`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ProcessOne(ctx); err == nil {
		t.Fatal("expected settlement failure")
	}
	balance(t, s, a.ID, 10000, 8800, 1200)
	var n int
	if err = s.DB.QueryRow(ctx, `SELECT count(*) FROM simulated_bank_receipts WHERE transfer_id=$1`, tr.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("durable receipt: %d %v", n, err)
	}
	if _, err = s.DB.Exec(ctx, `DROP TRIGGER test_failure ON outgoing_transfers`); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := s.ProcessOne(ctx); errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	balance(t, s, a.ID, 8800, 8800, 0)
	if err = s.DB.QueryRow(ctx, `SELECT count(*) FROM simulated_bank_receipts`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("duplicate external payment: %d %v", n, err)
	}
}
func TestPostgresMissingAndInvalidInputs(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.Open(ctx, " "); !errors.Is(err, ledger.ErrInvalid) {
		t.Fatalf("blank name: %v", err)
	}
	if _, err := s.Account(ctx, 999); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("missing account: %v", err)
	}
	if _, err := s.CreateTransfer(ctx, Request{Key: "missing", AccountID: 999, Destination: "SIM-1", Amount: 1}); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("missing transfer account: %v", err)
	}
}

func TestPostgresConcurrentSameKey(t *testing.T) {
	s := testStore(t)
	a := funded(t, s)
	ctx := context.Background()
	var wg sync.WaitGroup
	transfers := make(chan Transfer, 8)
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tr, err := s.CreateTransfer(ctx, Request{Key: "same-key", AccountID: a.ID, Destination: "SIM-001", Amount: 1200})
			transfers <- tr
			errs <- err
		}()
	}
	wg.Wait()
	close(transfers)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var id int64
	for tr := range transfers {
		if id == 0 {
			id = tr.ID
		}
		if tr.ID != id {
			t.Fatalf("different transfer IDs: %d %d", id, tr.ID)
		}
	}
	balance(t, s, a.ID, 10000, 8800, 1200)
}
