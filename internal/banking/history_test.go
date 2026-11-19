package banking

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"bank-ledger/internal/ledger"
	"bank-ledger/migrations"
)

func TestPostgresBeneficiariesAndHistory(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := funded(t, s)
	other, err := s.Open(ctx, "Other business")
	if err != nil {
		t.Fatal(err)
	}
	request := BeneficiaryRequest{Key: "supplier", Name: "Supplier", Destination: "SIM-SUPPLIER"}
	b, err := s.CreateBeneficiary(ctx, a.ID, request)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.CreateBeneficiary(ctx, a.ID, request)
	if err != nil || again.ID != b.ID {
		t.Fatalf("beneficiary replay: %+v %v", again, err)
	}
	request.Name = "Changed"
	if _, err = s.CreateBeneficiary(ctx, a.ID, request); !errors.Is(err, ledger.ErrConflict) {
		t.Fatalf("beneficiary conflict: %v", err)
	}
	// Same key is scoped to another account; the beneficiary itself cannot be used there.
	request.Name = "Other supplier"
	if _, err = s.CreateBeneficiary(ctx, other.ID, request); err != nil {
		t.Fatal(err)
	}
	r := Request{Key: "wrong-account", AccountID: other.ID, BeneficiaryID: &b.ID, Amount: 100}
	if _, err = s.CreateTransfer(ctx, r); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("cross-account beneficiary: %v", err)
	}
	r.AccountID = a.ID
	r.Destination = "SIM-OVERRIDE"
	if _, err = s.CreateTransfer(ctx, r); !errors.Is(err, ledger.ErrInvalid) {
		t.Fatalf("ambiguous destination: %v", err)
	}
	r.Destination = ""
	var ids []int64
	for i := 0; i < 3; i++ {
		r.Key = fmt.Sprintf("history-%d", i)
		tr, e := s.CreateTransfer(ctx, r)
		if e != nil {
			t.Fatal(e)
		}
		ids = append(ids, tr.ID)
		if tr.Destination != b.Destination || tr.BeneficiaryID == nil || *tr.BeneficiaryID != b.ID {
			t.Fatalf("snapshot: %+v", tr)
		}
	}
	tr, err := s.CreateTransfer(ctx, r)
	if err != nil || tr.ID != ids[2] {
		t.Fatalf("beneficiary transfer replay: %+v %v", tr, err)
	}
	// Switching from a beneficiary to its raw destination is a different request.
	r.BeneficiaryID = nil
	r.Destination = b.Destination
	if _, err = s.CreateTransfer(ctx, r); !errors.Is(err, ledger.ErrConflict) {
		t.Fatalf("changed request identity: %v", err)
	}
	page, err := s.Transfers(ctx, a.ID, 0, 2)
	if err != nil || len(page.Items) != 2 || page.NextBeforeID == nil {
		t.Fatalf("first page: %+v %v", page, err)
	}
	if page.Items[0].ID != ids[2] || page.Items[1].ID != ids[1] {
		t.Fatalf("page order: %+v", page)
	}
	second, err := s.Transfers(ctx, a.ID, *page.NextBeforeID, 2)
	if err != nil || len(second.Items) != 1 || second.Items[0].ID != ids[0] || second.NextBeforeID != nil {
		t.Fatalf("second page: %+v %v", second, err)
	}
	empty, err := s.Transfers(ctx, other.ID, 0, 50)
	if err != nil || len(empty.Items) != 0 || empty.Items == nil {
		t.Fatalf("account-scoped history: %+v %v", empty, err)
	}
	beneficiaries, err := s.Beneficiaries(ctx, a.ID, 0, 50)
	if err != nil || len(beneficiaries.Items) != 1 || beneficiaries.Items[0].ID != b.ID {
		t.Fatalf("beneficiary list: %+v %v", beneficiaries, err)
	}
	for i := 0; i < 2; i++ {
		request.Key = fmt.Sprintf("more-%d", i)
		if _, err = s.CreateBeneficiary(ctx, a.ID, request); err != nil {
			t.Fatal(err)
		}
	}
	beneficiaries, err = s.Beneficiaries(ctx, a.ID, 0, 2)
	if err != nil || len(beneficiaries.Items) != 2 || beneficiaries.NextBeforeID == nil {
		t.Fatalf("beneficiary page: %+v %v", beneficiaries, err)
	}
	rest, err := s.Beneficiaries(ctx, a.ID, *beneficiaries.NextBeforeID, 2)
	if err != nil || len(rest.Items) != 1 || rest.Items[0].ID != b.ID || rest.NextBeforeID != nil {
		t.Fatalf("beneficiary continuation: %+v %v", rest, err)
	}
	if _, err = s.DB.Exec(ctx, `UPDATE beneficiaries SET destination='CHANGED' WHERE id=$1`, b.ID); err == nil {
		t.Fatal("beneficiary should be immutable")
	}
	balance(t, s, a.ID, 10000, 9700, 300)
}
func assertTimeline(t *testing.T, s *Store, id int64, kinds ...string) {
	t.Helper()
	timeline, err := s.Timeline(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(timeline.Events) != len(kinds) {
		t.Fatalf("timeline: %+v", timeline)
	}
	for i, e := range timeline.Events {
		if e.Kind != kinds[i] || e.OccurredAt.IsZero() {
			t.Fatalf("event %d: %+v", i, e)
		}
		if e.Kind == "funds_reserved" || e.Kind == "transfer_settled" {
			if e.JournalID == nil {
				t.Fatal("missing accounting link")
			}
		}
		if e.Kind == "bank_confirmed" && e.ProviderReference == nil {
			t.Fatal("missing provider reference")
		}
	}
}
func TestPostgresTimelineAcrossSettlementFailure(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := funded(t, s)
	tr, err := s.CreateTransfer(ctx, Request{Key: "timeline", AccountID: a.ID, Destination: "SIM-1", Amount: 100})
	if err != nil {
		t.Fatal(err)
	}
	assertTimeline(t, s, tr.ID, "transfer_created", "funds_reserved")
	_, err = s.DB.Exec(ctx, `CREATE FUNCTION fail_settlement() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'test failure'; END $$; CREATE TRIGGER test_failure BEFORE UPDATE ON outgoing_transfers FOR EACH ROW EXECUTE FUNCTION fail_settlement()`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ProcessOne(ctx); err == nil {
		t.Fatal("expected settlement failure")
	}
	assertTimeline(t, s, tr.ID, "transfer_created", "funds_reserved", "bank_confirmed")
	if _, err = s.DB.Exec(ctx, `DROP TRIGGER test_failure ON outgoing_transfers`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ProcessOne(ctx); err != nil {
		t.Fatal(err)
	}
	assertTimeline(t, s, tr.ID, "transfer_created", "funds_reserved", "bank_confirmed", "transfer_settled")
	if _, err = s.ProcessOne(ctx); err != nil {
		t.Fatal(err)
	}
	assertTimeline(t, s, tr.ID, "transfer_created", "funds_reserved", "bank_confirmed", "transfer_settled")
	if _, err = s.Timeline(ctx, 999999); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("missing timeline: %v", err)
	}
	if _, err = s.Transfers(ctx, a.ID, 0, 101); !errors.Is(err, ledger.ErrInvalid) {
		t.Fatalf("oversized page: %v", err)
	}
	if _, err = s.Beneficiaries(ctx, 999999, 0, 50); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("missing account list: %v", err)
	}
}
func TestPostgresUpgradePreservesLegacyTransfers(t *testing.T) {
	s := testStore(t, 3)
	ctx := context.Background()
	a := funded(t, s)
	// Pre-migration code wrote this shape, without a beneficiary column.
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(tx)
	var id int64
	if err = tx.QueryRow(ctx, `INSERT INTO outgoing_transfers(idempotency_key,account_id,destination,amount) VALUES('legacy',$1,'SIM-LEGACY',100) RETURNING id`, a.ID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	_, available, reserved, err := backing(ctx, tx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := ledger.PostTx(ctx, tx, ledger.Posting{Key: fmt.Sprintf("bank:reserve:%d", id), Description: "Reserve outgoing transfer", Entries: []ledger.Entry{{AccountID: available, Side: "debit", Amount: 100}, {AccountID: reserved, Side: "credit", Amount: 100}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE outgoing_transfers SET reserve_journal_id=$2 WHERE id=$1`, id, journal.ID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := migrations.Apply(ctx, s.DB); err != nil {
			t.Fatal(err)
		}
	}
	tr, err := s.Transfer(ctx, id)
	if err != nil || tr.Destination != "SIM-LEGACY" || tr.BeneficiaryID != nil {
		t.Fatalf("legacy transfer: %+v %v", tr, err)
	}
	assertTimeline(t, s, id, "transfer_created", "funds_reserved")
	if _, err = s.ProcessOne(ctx); err != nil {
		t.Fatal(err)
	}
	balance(t, s, a.ID, 9900, 9900, 0)
	assertTimeline(t, s, id, "transfer_created", "funds_reserved", "bank_confirmed", "transfer_settled")
	if _, err = s.CreateBeneficiary(ctx, a.ID, BeneficiaryRequest{Key: "new", Name: "New", Destination: "SIM-NEW"}); err != nil {
		t.Fatal(err)
	}
}
