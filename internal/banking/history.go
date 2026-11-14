package banking

import (
	"context"
	"errors"
	"strings"
	"time"

	"bank-ledger/internal/ledger"
	"github.com/jackc/pgx/v5"
)

type BeneficiaryRequest struct {
	Key         string `json:"idempotency_key"`
	Name        string `json:"name"`
	Destination string `json:"destination"`
}
type Beneficiary struct {
	ID          int64     `json:"id"`
	AccountID   int64     `json:"account_id"`
	Name        string    `json:"name"`
	Destination string    `json:"destination"`
	CreatedAt   time.Time `json:"created_at"`
}

const beneficiaryColumns = `id,account_id,name,destination,created_at`

func scanBeneficiary(row pgx.Row) (b Beneficiary, err error) {
	err = row.Scan(&b.ID, &b.AccountID, &b.Name, &b.Destination, &b.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ledger.ErrNotFound
	}
	return
}
func (s *Store) CreateBeneficiary(ctx context.Context, accountID int64, r BeneficiaryRequest) (Beneficiary, error) {
	if accountID <= 0 || !validKey(r.Key) || strings.TrimSpace(r.Name) == "" || len(r.Name) > 200 || strings.TrimSpace(r.Destination) == "" || len(r.Destination) > 100 {
		return Beneficiary{}, ledger.ErrInvalid
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return Beneficiary{}, err
	}
	defer rollback(tx)
	b, err := scanBeneficiary(tx.QueryRow(ctx, `INSERT INTO beneficiaries(account_id,idempotency_key,name,destination) SELECT id,$2,$3,$4 FROM bank_accounts WHERE id=$1 ON CONFLICT(account_id,idempotency_key) DO NOTHING RETURNING `+beneficiaryColumns, accountID, r.Key, r.Name, r.Destination))
	if errors.Is(err, ledger.ErrNotFound) {
		b, err = scanBeneficiary(tx.QueryRow(ctx, `SELECT `+beneficiaryColumns+` FROM beneficiaries WHERE account_id=$1 AND idempotency_key=$2`, accountID, r.Key))
		if err == nil && (b.Name != r.Name || b.Destination != r.Destination) {
			return Beneficiary{}, ledger.ErrConflict
		}
	}
	if err != nil {
		return Beneficiary{}, err
	}
	return b, tx.Commit(ctx)
}

// Pages use descending identity order, not commit or settlement order. A cursor
// continues after the last returned ID; it is not a cross-request snapshot.
type TransferPage struct {
	Items        []Transfer `json:"items"`
	NextBeforeID *int64     `json:"next_before_id"`
}
type BeneficiaryPage struct {
	Items        []Beneficiary `json:"items"`
	NextBeforeID *int64        `json:"next_before_id"`
}

func validPage(accountID, before int64, limit int) bool {
	return accountID > 0 && before >= 0 && limit >= 1 && limit <= 100
}
func (s *Store) requireAccount(ctx context.Context, id int64) error {
	var exists bool
	if err := s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM bank_accounts WHERE id=$1)`, id).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ledger.ErrNotFound
	}
	return nil
}
func (s *Store) Transfers(ctx context.Context, accountID, before int64, limit int) (TransferPage, error) {
	page := TransferPage{Items: []Transfer{}}
	if !validPage(accountID, before, limit) {
		return page, ledger.ErrInvalid
	}
	if err := s.requireAccount(ctx, accountID); err != nil {
		return page, err
	}
	rows, err := s.DB.Query(ctx, `SELECT `+transferColumns+` FROM outgoing_transfers WHERE account_id=$1 AND ($2::bigint=0 OR id<$2) ORDER BY id DESC LIMIT $3`, accountID, before, limit+1)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		t, err := scanTransfer(rows)
		if err != nil {
			return page, err
		}
		page.Items = append(page.Items, t)
	}
	if err = rows.Err(); err != nil {
		return page, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		id := page.Items[limit-1].ID
		page.NextBeforeID = &id
	}
	return page, nil
}
func (s *Store) Beneficiaries(ctx context.Context, accountID, before int64, limit int) (BeneficiaryPage, error) {
	page := BeneficiaryPage{Items: []Beneficiary{}}
	if !validPage(accountID, before, limit) {
		return page, ledger.ErrInvalid
	}
	if err := s.requireAccount(ctx, accountID); err != nil {
		return page, err
	}
	rows, err := s.DB.Query(ctx, `SELECT `+beneficiaryColumns+` FROM beneficiaries WHERE account_id=$1 AND ($2::bigint=0 OR id<$2) ORDER BY id DESC LIMIT $3`, accountID, before, limit+1)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		b, err := scanBeneficiary(rows)
		if err != nil {
			return page, err
		}
		page.Items = append(page.Items, b)
	}
	if err = rows.Err(); err != nil {
		return page, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		id := page.Items[limit-1].ID
		page.NextBeforeID = &id
	}
	return page, nil
}

type Event struct {
	Kind              string    `json:"kind"`
	OccurredAt        time.Time `json:"occurred_at"`
	JournalID         *int64    `json:"journal_id,omitempty"`
	ProviderReference *int64    `json:"provider_reference,omitempty"`
}
type Timeline struct {
	TransferID int64   `json:"transfer_id"`
	Events     []Event `json:"events"`
}

// Timeline derives committed facts in one statement, including a receipt that
// survived local settlement rollback. It is not a log of unsuccessful attempts.
func (s *Store) Timeline(ctx context.Context, id int64) (Timeline, error) {
	result := Timeline{TransferID: id, Events: []Event{}}
	if id <= 0 {
		return result, ledger.ErrInvalid
	}
	rows, err := s.DB.Query(ctx, `
 WITH t AS (SELECT * FROM outgoing_transfers WHERE id=$1), facts AS (
 SELECT 1 AS step,'transfer_created'::text AS kind,t.created_at AS occurred_at,NULL::bigint AS journal_id,NULL::bigint AS provider_reference FROM t
 UNION ALL
 SELECT 2,'funds_reserved',j.created_at,j.id,NULL::bigint FROM t JOIN journals j ON j.id=t.reserve_journal_id
 UNION ALL
 SELECT 3,'bank_confirmed',r.created_at,NULL::bigint,r.transfer_id FROM t JOIN simulated_bank_receipts r ON r.transfer_id=t.id
 UNION ALL
 SELECT 4,'transfer_settled',t.settled_at,t.settlement_journal_id,NULL::bigint FROM t WHERE t.status='settled'
 ) SELECT kind,occurred_at,journal_id,provider_reference FROM facts ORDER BY step`, id)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var e Event
		if err = rows.Scan(&e.Kind, &e.OccurredAt, &e.JournalID, &e.ProviderReference); err != nil {
			return result, err
		}
		result.Events = append(result.Events, e)
	}
	if err = rows.Err(); err != nil {
		return result, err
	}
	if len(result.Events) == 0 {
		return result, ledger.ErrNotFound
	}
	return result, nil
}
