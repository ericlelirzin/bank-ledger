package ledger

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"bank-ledger/migrations"

	"github.com/jackc/pgx/v5/pgconn"
)

// requirePG checks both SQLSTATE and diagnostic text so a test cannot pass
// because some unrelated database constraint failed first.
func requirePG(t testing.TB, err error, code, detail string) {
	t.Helper()
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != code || !strings.Contains(pg.Message, detail) {
		t.Fatalf("want %s %q; got %v", code, detail, err)
	}
}

// Wait for the database's actual wait state, not an arbitrary sleep. Each
// fixture owns its pool/schema; unrelated test queries cannot satisfy this.
func waitBlocked(t testing.TB, s *Store) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var n int
		err := s.DB.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE pid<>pg_backend_pid() AND state='active' AND wait_event_type='Lock' AND query LIKE '%journals%' AND query NOT LIKE '%pg_stat_activity%' AND application_name=$1`, s.DB.Config().ConnConfig.RuntimeParams["application_name"]).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("request did not reach lock wait")
		case <-ticker.C:
		}
	}
}

// TestPostgresInitialKeyHolderRollback makes one transaction hold a key, then
// roll back. A waiting request must acquire that released key and post
// successfully.
func TestPostgresInitialKeyHolderRollback(t *testing.T) {
	s := testStore(t)
	a, b, _ := setupAccounts(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	var id int64
	if e = tx.QueryRow(ctx, `INSERT INTO journals(idempotency_key,request_hash,description) VALUES('contested','x','') RETURNING id`).Scan(&id); e != nil {
		t.Fatal(e)
	}
	// Stage a balanced movement in the first transaction, then abandon it.
	if _, e = tx.Exec(ctx, `INSERT INTO entries VALUES($1,1,$2,'debit',7),($1,2,$3,'credit',7)`, id, b.ID, a.ID); e != nil {
		t.Fatal(e)
	}
	result := make(chan error, 1)
	go func() {
		_, e := s.Transfer(ctx, Transfer{Key: "contested", From: a.ID, To: b.ID, Amount: 3})
		result <- e
	}()
	waitBlocked(t, s)
	if e = tx.Rollback(ctx); e != nil {
		t.Fatal(e)
	}
	if e = <-result; e != nil {
		t.Fatal(e)
	}
	bb, e := s.Account(ctx, b.ID)
	if e != nil || bb.Balance != 3 {
		t.Fatalf("abandoned posting leaked: %+v %v", bb, e)
	}
	var n int
	if e = s.DB.QueryRow(ctx, `SELECT count(*) FROM journals WHERE idempotency_key='contested'`).Scan(&n); e != nil || n != 1 {
		t.Fatal(n, e)
	}
}

// TestPostgresCancelledKeyWaiter cancels a request blocked on a key and
// verifies that cancellation does not reserve the key or prevent a later
// successful retry.
func TestPostgresCancelledKeyWaiter(t *testing.T) {
	s := testStore(t)
	a, b, _ := setupAccounts(t, s)
	ctx := context.Background()
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, `INSERT INTO journals(idempotency_key,request_hash,description) VALUES('cancel-wait','x','')`); e != nil {
		t.Fatal(e)
	}
	waiting, cancel := context.WithCancel(ctx)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, e := s.Transfer(waiting, Transfer{Key: "cancel-wait", From: a.ID, To: b.ID, Amount: 2})
		result <- e
	}()
	waitBlocked(t, s)
	cancel()
	select {
	case e = <-result:
		if e == nil {
			t.Fatal("cancelled waiter succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation hung")
	}
	if e = tx.Rollback(ctx); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Transfer(ctx, Transfer{Key: "cancel-wait", From: a.ID, To: b.ID, Amount: 2}); e != nil {
		t.Fatal(e)
	}
}

// TestPostgresConcurrentConflictingKeys submits different payloads with one key
// while locks control the timing. Exactly one payload can commit; the other
// must conflict.
func TestPostgresConcurrentConflictingKeys(t *testing.T) {
	s := testStore(t)
	a, b, _ := setupAccounts(t, s)
	ctx := context.Background()
	lock, e := s.DB.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer lock.Rollback(ctx)
	if _, e = lock.Exec(ctx, `SELECT id FROM accounts WHERE id=$1 FOR UPDATE`, a.ID); e != nil {
		t.Fatal(e)
	}
	result := make(chan error, 2)
	go func() {
		_, e := s.Transfer(ctx, Transfer{Key: "different", From: a.ID, To: b.ID, Amount: 2})
		result <- e
	}()
	// Its header exists, but its account lock is blocked.
	deadline := time.After(3 * time.Second)
	for {
		var n int
		e = s.DB.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type='Lock' AND query LIKE 'SELECT id,code%' AND application_name=$1`, s.DB.Config().ConnConfig.RuntimeParams["application_name"]).Scan(&n)
		if e != nil {
			t.Fatal(e)
		}
		if n > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("no account waiter")
		case <-time.After(5 * time.Millisecond):
		}
	}
	go func() {
		_, e := s.Transfer(ctx, Transfer{Key: "different", From: a.ID, To: b.ID, Amount: 3})
		result <- e
	}()
	waitBlocked(t, s)
	if e = lock.Rollback(ctx); e != nil {
		t.Fatal(e)
	}
	success, conflict := 0, 0
	for i := 0; i < 2; i++ {
		e = <-result
		if e == nil {
			success++
		} else if errors.Is(e, ErrConflict) {
			conflict++
		} else {
			t.Fatal(e)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatal(success, conflict)
	}
}

// TestPostgresIndividualInvariants isolates late append, totals, currency, and
// empty-journal failures so the intended invariant is the reason each write is
// rejected.
func TestPostgresIndividualInvariants(t *testing.T) {
	s := testStore(t)
	a, b, _ := setupAccounts(t, s)
	ctx := context.Background()
	eur, e := s.CreateAccount(ctx, Account{Code: "e", Name: "Euro", Kind: "asset", Currency: "EUR", NormalSide: "debit"})
	if e != nil {
		t.Fatal(e)
	}
	t.Run("balanced late append", func(t *testing.T) {
		_, e := s.DB.Exec(ctx, `INSERT INTO entries SELECT j.id,v.* FROM journals j CROSS JOIN (VALUES (9,$1::bigint,'debit',1::bigint),(10,$2::bigint,'credit',1::bigint)) v(line,account_id,side,amount) WHERE j.idempotency_key='fund'`, b.ID, a.ID)
		requirePG(t, e, "23514", "cannot append")
	})
	for _, tc := range []struct {
		name    string
		entries []Entry
	}{
		{"sum", []Entry{{a.ID, "debit", 2}, {b.ID, "credit", 1}}},
		{"currency", []Entry{{a.ID, "debit", 1}, {eur.ID, "credit", 1}}},
		{"empty", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, e := s.DB.Begin(ctx)
			if e != nil {
				t.Fatal(e)
			}
			defer tx.Rollback(ctx)
			var id int64
			if e = tx.QueryRow(ctx, `INSERT INTO journals(idempotency_key,request_hash,description) VALUES($1,'x','') RETURNING id`, tc.name).Scan(&id); e != nil {
				t.Fatal(e)
			}
			// All accounts can receive these lines without failing overdraft first.
			if tc.name == "sum" {
				tc.entries = []Entry{{b.ID, "debit", 2}, {a.ID, "credit", 1}}
			}
			if tc.name == "currency" {
				tc.entries = []Entry{{eur.ID, "debit", 1}, {a.ID, "credit", 1}}
			}
			for i, v := range tc.entries {
				if _, e = tx.Exec(ctx, `INSERT INTO entries VALUES($1,$2,$3,$4,$5)`, id, i+1, v.AccountID, v.Side, v.Amount); e != nil {
					t.Fatal(e)
				}
			}
			requirePG(t, tx.Commit(ctx), "23514", "journal must have")
		})
	}
	t.Run("queue tampering", func(t *testing.T) {
		_, e := s.DB.Exec(ctx, `INSERT INTO pending_journals SELECT id FROM journals LIMIT 1`)
		requirePG(t, e, "23514", "queue is internal")
	})
	var pending int
	if e = s.DB.QueryRow(ctx, `SELECT count(*) FROM pending_journals`).Scan(&pending); e != nil || pending != 0 {
		t.Fatal(pending, e)
	}
}

// TestPostgresIntegerBoundaries distinguishes large aggregate totals from
// individual balance limits, including exact MinInt64 and rejection of
// transient overflow.
func TestPostgresIntegerBoundaries(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	create := func(code string) Account {
		a, e := s.CreateAccount(ctx, Account{Code: code, Name: code, Kind: "asset", Currency: "USD", NormalSide: "debit", AllowOverdraft: true})
		if e != nil {
			t.Fatal(e)
		}
		return a
	}
	a, b, c, d := create("a"), create("b"), create("c"), create("d")
	// Each balance fits while both debit and credit totals exceed int64.
	_, e := s.Post(ctx, Posting{Key: "large", Entries: []Entry{{a.ID, "debit", math.MaxInt64}, {b.ID, "debit", math.MaxInt64}, {c.ID, "credit", math.MaxInt64}, {d.ID, "credit", math.MaxInt64}}})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Post(ctx, Posting{Key: "min", Entries: []Entry{{a.ID, "debit", 1}, {c.ID, "credit", 1}}}); e == nil {
		t.Fatal("upper overflow accepted")
	}
	// Use a fresh receiver, so the first line succeeds before reaching MinInt64.
	receiver := create("receiver")
	if _, e = s.Post(ctx, Posting{Key: "min", Entries: []Entry{{receiver.ID, "debit", 1}, {c.ID, "credit", 1}}}); e != nil {
		t.Fatal(e)
	}
	cc, e := s.Account(ctx, c.ID)
	if e != nil || cc.Balance != math.MinInt64 {
		t.Fatal(cc, e)
	}
	_, e = s.Post(ctx, Posting{Key: "underflow", Entries: []Entry{{receiver.ID, "debit", 1}, {c.ID, "credit", 1}}})
	requirePG(t, e, "22003", "out of range")
	r, e := s.Account(ctx, receiver.ID)
	if e != nil || r.Balance != 1 {
		t.Fatalf("underflow rollback: %+v %v", r, e)
	}
	// Conservative, documented policy: intermediate overflow is rejected even if net is zero.
	_, e = s.Post(ctx, Posting{Key: "net-zero", Entries: []Entry{{a.ID, "credit", 1}, {a.ID, "debit", 1}}})
	requirePG(t, e, "22003", "out of range")
}

// TestPostgresCancellationAfterAppliedEntry pauses after an earlier entry
// already changed a balance inside the transaction. Cancellation must roll back
// that partial work too.
func TestPostgresCancellationAfterAppliedEntry(t *testing.T) {
	s := testStore(t)
	a, b, _ := setupAccounts(t, s)
	ctx := context.Background()
	// A test-only fault waits on the source line after the destination line has
	// already been inserted and its balance trigger has executed.
	_, e := s.DB.Exec(ctx, `CREATE FUNCTION pause_source() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.side='credit' THEN PERFORM pg_sleep(10); END IF; RETURN NEW; END $$;CREATE TRIGGER entries_zz_pause AFTER INSERT ON entries FOR EACH ROW EXECUTE FUNCTION pause_source()`)
	if e != nil {
		t.Fatal(e)
	}
	waiting, cancel := context.WithCancel(ctx)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, e := s.Transfer(waiting, Transfer{Key: "partial-cancel", From: a.ID, To: b.ID, Amount: 3})
		result <- e
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var n int
		e = s.DB.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE application_name=$1 AND wait_event='PgSleep'`, s.DB.Config().ConnConfig.RuntimeParams["application_name"]).Scan(&n)
		if e != nil {
			t.Fatal(e)
		}
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("did not reach staged entry")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case e = <-result:
		if e == nil {
			t.Fatal("cancelled posting succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled query hung")
	}
	bb, e := s.Account(ctx, b.ID)
	if e != nil || bb.Balance != 0 {
		t.Fatalf("partial entry survived: %+v %v", bb, e)
	}
	var n int
	if e = s.DB.QueryRow(ctx, `SELECT count(*) FROM journals WHERE idempotency_key='partial-cancel'`).Scan(&n); e != nil || n != 0 {
		t.Fatal(n, e)
	}
	if _, e = s.DB.Exec(ctx, `DROP TRIGGER entries_zz_pause ON entries;DROP FUNCTION pause_source()`); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Transfer(ctx, Transfer{Key: "partial-cancel", From: a.ID, To: b.ID, Amount: 3}); e != nil {
		t.Fatal(e)
	}
}

// TestPostgresQueueSurvivesSavepointRollback validates, rolls back to a
// savepoint, then appends. The restored queue marker must still enforce the
// final journal check.
func TestPostgresQueueSurvivesSavepointRollback(t *testing.T) {
	s := testStore(t)
	a, b, _ := setupAccounts(t, s)
	ctx := context.Background()
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	var id int64
	if e = tx.QueryRow(ctx, `INSERT INTO journals(idempotency_key,request_hash,description) VALUES('savepoint','x','') RETURNING id`).Scan(&id); e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(ctx, `INSERT INTO entries VALUES($1,1,$2,'debit',1),($1,2,$3,'credit',1)`, id, b.ID, a.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(ctx, `SAVEPOINT validated; SET CONSTRAINTS ALL IMMEDIATE; ROLLBACK TO SAVEPOINT validated;SET CONSTRAINTS ALL DEFERRED`); e != nil {
		t.Fatal(e)
	}
	var n int
	if e = tx.QueryRow(ctx, `SELECT count(*) FROM pending_journals WHERE journal_id=$1`, id).Scan(&n); e != nil || n != 1 {
		t.Fatal(n, e)
	}
	if _, e = tx.Exec(ctx, `INSERT INTO entries VALUES($1,3,$2,'debit',1)`, id, b.ID); e != nil {
		t.Fatal(e)
	}
	requirePG(t, tx.Commit(ctx), "23514", "journal must have")
}

// TestPostgresUpgradeFromV1 applies 002 over populated v1 data, checking that
// balances survive and new postings work without resetting the schema.
func TestPostgresUpgradeFromV1(t *testing.T) {
	s := testStoreAtVersion(t, 1)
	a, b, _ := setupAccounts(t, s)
	ctx := context.Background()
	if e := migrations.Apply(ctx, s.DB); e != nil {
		t.Fatal(e)
	}
	if e := migrations.Apply(ctx, s.DB); e != nil {
		t.Fatal(e)
	}
	aa, e := s.Account(ctx, a.ID)
	if e != nil || aa.Balance != 100 {
		t.Fatal(aa, e)
	}
	if _, e = s.Transfer(ctx, Transfer{Key: "after-upgrade", From: a.ID, To: b.ID, Amount: 2}); e != nil {
		t.Fatal(e)
	}
	var version int
	if e = s.DB.QueryRow(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&version); e != nil || version != 2 {
		t.Fatal(version, e)
	}
}

// TestPostgresValidationOncePerDirtyJournal counts actual marker drains. Many
// lines should share one aggregate validation until an explicit constraint
// flush starts a new epoch.
func TestPostgresValidationOncePerDirtyJournal(t *testing.T) {
	s := testStore(t)
	a, _, _ := setupAccounts(t, s)
	ctx := context.Background()
	// Test-only observation of queue drains. No accounting trigger is replaced.
	_, e := s.DB.Exec(ctx, `CREATE TABLE validation_counts(journal_id bigint);CREATE FUNCTION count_validations() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN INSERT INTO validation_counts VALUES(OLD.journal_id);RETURN OLD;END $$;CREATE TRIGGER pending_count AFTER DELETE ON pending_journals FOR EACH ROW EXECUTE FUNCTION count_validations()`)
	if e != nil {
		t.Fatal(e)
	}
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	var id int64
	if e = tx.QueryRow(ctx, `INSERT INTO journals(idempotency_key,request_hash,description) VALUES('once','x','') RETURNING id`).Scan(&id); e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(ctx, `INSERT INTO entries SELECT $1,g,$2,CASE WHEN g%2=0 THEN 'debit' ELSE 'credit' END,1 FROM generate_series(1,1000) g`, id, a.ID); e != nil {
		t.Fatal(e)
	}
	var n int
	if e = tx.QueryRow(ctx, `SELECT count(*) FROM pending_journals`).Scan(&n); e != nil || n != 1 {
		t.Fatal(n, e)
	}
	if _, e = tx.Exec(ctx, `SET CONSTRAINTS ALL IMMEDIATE;SET CONSTRAINTS ALL DEFERRED`); e != nil {
		t.Fatal(e)
	}
	// A fresh marker must be scheduled after early validation, once for both lines.
	if _, e = tx.Exec(ctx, `INSERT INTO entries VALUES($1,1001,$2,'debit',1),($1,1002,$2,'credit',1)`, id, a.ID); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	if e = s.DB.QueryRow(ctx, `SELECT count(*) FROM validation_counts WHERE journal_id=$1`, id).Scan(&n); e != nil || n != 2 {
		t.Fatalf("want two validation epochs; got %d %v", n, e)
	}
}

// TestPostgresEntryChecksAndQueueProtection directly tests amount, side,
// foreign-key, and queue-tampering failures with their expected SQLSTATE
// diagnostics.
func TestPostgresEntryChecksAndQueueProtection(t *testing.T) {
	s := testStore(t)
	a, _, _ := setupAccounts(t, s)
	ctx := context.Background()
	for _, tc := range []struct {
		name, side, code, detail string
		account, amount          int64
	}{
		{"zero", "debit", "23514", "entries_amount_check", a.ID, 0},
		{"negative", "debit", "23514", "entries_amount_check", a.ID, -1},
		{"side", "invalid", "23514", "entries_side_check", a.ID, 1},
		{"account", "debit", "23503", "entries_account_id_fkey", 999999, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, e := s.DB.Begin(ctx)
			if e != nil {
				t.Fatal(e)
			}
			defer tx.Rollback(ctx)
			var id int64
			if e = tx.QueryRow(ctx, `INSERT INTO journals(idempotency_key,request_hash,description) VALUES($1,'x','') RETURNING id`, tc.name).Scan(&id); e != nil {
				t.Fatal(e)
			}
			_, e = tx.Exec(ctx, `INSERT INTO entries VALUES($1,1,$2,$3,$4)`, id, tc.account, tc.side, tc.amount)
			requirePG(t, e, tc.code, tc.detail)
		})
	}
	for _, operation := range []string{`DELETE FROM pending_journals`, `UPDATE pending_journals SET journal_id=journal_id`} {
		tx, e := s.DB.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = tx.Exec(ctx, `INSERT INTO journals(idempotency_key,request_hash,description) VALUES('queue-change','x','')`); e != nil {
			t.Fatal(e)
		}
		_, e = tx.Exec(ctx, operation)
		requirePG(t, e, "23514", "queue is internal")
		_ = tx.Rollback(ctx)
	}
}
