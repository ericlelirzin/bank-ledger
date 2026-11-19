// Package banking owns simulated USD product accounts and outgoing transfers.
package banking

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"bank-ledger/internal/ledger"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct{ DB *pgxpool.Pool }
type Account struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Currency  string `json:"currency"`
	Posted    int64  `json:"posted_balance"`
	Available int64  `json:"available_balance"`
	Reserved  int64  `json:"reserved_balance"`
}
type Request struct {
	Key           string `json:"idempotency_key"`
	AccountID     int64  `json:"account_id"`
	Destination   string `json:"destination,omitempty"`
	BeneficiaryID *int64 `json:"beneficiary_id,omitempty"`
	Amount        int64  `json:"amount"`
}
type Transfer struct {
	BeneficiaryID       *int64     `json:"beneficiary_id,omitempty"`
	ID                  int64      `json:"id"`
	AccountID           int64      `json:"account_id"`
	Destination         string     `json:"destination"`
	Amount              int64      `json:"amount"`
	Status              string     `json:"status"`
	ReserveJournalID    *int64     `json:"reserve_journal_id"`
	SettlementJournalID *int64     `json:"settlement_journal_id"`
	CreatedAt           time.Time  `json:"created_at"`
	SettledAt           *time.Time `json:"settled_at"`
}

const transferColumns = `id,account_id,destination,amount,status,reserve_journal_id,settlement_journal_id,created_at,settled_at,beneficiary_id`

func scanTransfer(row pgx.Row) (t Transfer, err error) {
	err = row.Scan(&t.ID, &t.AccountID, &t.Destination, &t.Amount, &t.Status, &t.ReserveJournalID, &t.SettlementJournalID, &t.CreatedAt, &t.SettledAt, &t.BeneficiaryID)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ledger.ErrNotFound
	}
	return
}
func rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}
func validKey(key string) bool { return strings.TrimSpace(key) != "" && len(key) <= 200 }

// Open creates the product and all backing ledger accounts atomically.
func (s *Store) Open(ctx context.Context, name string) (Account, error) {
	if strings.TrimSpace(name) == "" || len(name) > 200 {
		return Account{}, ledger.ErrInvalid
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return Account{}, err
	}
	defer rollback(tx)
	var id int64
	err = tx.QueryRow(ctx, `SELECT nextval(pg_get_serial_sequence('bank_accounts','id'))`).Scan(&id)
	if err != nil {
		return Account{}, err
	}
	ids := make([]int64, 3)
	for i, role := range []string{"cash", "available", "reserved"} {
		kind, side := "liability", "credit"
		if i == 0 {
			kind, side = "asset", "debit"
		}
		err = tx.QueryRow(ctx, `INSERT INTO accounts(code,name,kind,currency,normal_side) VALUES($1,$2,$3,'USD',$4) RETURNING id`, fmt.Sprintf("bank:%d:%s", id, role), fmt.Sprintf("Bank account %d %s", id, role), kind, side).Scan(&ids[i])
		if err != nil {
			return Account{}, err
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO bank_accounts(id,name,cash_id,available_id,reserved_id) OVERRIDING SYSTEM VALUE VALUES($1,$2,$3,$4,$5)`, id, name, ids[0], ids[1], ids[2])
	if err != nil {
		return Account{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Account{}, err
	}
	return Account{ID: id, Name: name, Currency: "USD"}, nil
}
func (s *Store) Account(ctx context.Context, id int64) (a Account, err error) {
	// A single statement provides a consistent view of all three balances.
	err = s.DB.QueryRow(ctx, `SELECT b.id,b.name,b.currency,c.balance,a.balance,r.balance FROM bank_accounts b JOIN accounts c ON c.id=b.cash_id JOIN accounts a ON a.id=b.available_id JOIN accounts r ON r.id=b.reserved_id WHERE b.id=$1`, id).Scan(&a.ID, &a.Name, &a.Currency, &a.Posted, &a.Available, &a.Reserved)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ledger.ErrNotFound
	}
	return
}
func backing(ctx context.Context, tx pgx.Tx, id int64) (cash, available, reserved int64, err error) {
	err = tx.QueryRow(ctx, `SELECT cash_id,available_id,reserved_id FROM bank_accounts WHERE id=$1`, id).Scan(&cash, &available, &reserved)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ledger.ErrNotFound
	}
	return
}

// Fund is a development-only simulated inbound payment, never a real bank deposit.
func (s *Store) Fund(ctx context.Context, id, amount int64, key string) (ledger.Result, error) {
	if id <= 0 || amount <= 0 || !validKey(key) {
		return ledger.Result{}, ledger.ErrInvalid
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return ledger.Result{}, err
	}
	defer rollback(tx)
	cash, available, _, err := backing(ctx, tx, id)
	if err != nil {
		return ledger.Result{}, err
	}
	// Hash the user key into a bounded, operation-scoped internal key.
	res, err := ledger.PostTx(ctx, tx, ledger.Posting{Key: fundKey(key), Description: "Simulated inbound funding", Entries: []ledger.Entry{{AccountID: cash, Side: "debit", Amount: amount}, {AccountID: available, Side: "credit", Amount: amount}}})
	if err != nil {
		return ledger.Result{}, err
	}
	return res, tx.Commit(ctx)
}
func (s *Store) CreateTransfer(ctx context.Context, r Request) (Transfer, error) {
	if !validKey(r.Key) || r.AccountID <= 0 || r.Amount <= 0 || !validDestination(r) {
		return Transfer{}, ledger.ErrInvalid
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return Transfer{}, err
	}
	defer rollback(tx)
	if r.BeneficiaryID != nil {
		err = tx.QueryRow(ctx, `SELECT destination FROM beneficiaries WHERE account_id=$1 AND id=$2`, r.AccountID, *r.BeneficiaryID).Scan(&r.Destination)
		if errors.Is(err, pgx.ErrNoRows) {
			return Transfer{}, ledger.ErrNotFound
		}
		if err != nil {
			return Transfer{}, err
		}
	}
	var id int64
	err = tx.QueryRow(ctx, `INSERT INTO outgoing_transfers(idempotency_key,account_id,destination,amount,beneficiary_id) SELECT $1,id,$3,$4,$5 FROM bank_accounts WHERE id=$2 ON CONFLICT(idempotency_key) DO NOTHING RETURNING id`, r.Key, r.AccountID, r.Destination, r.Amount, r.BeneficiaryID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		t, e := scanTransfer(tx.QueryRow(ctx, `SELECT `+transferColumns+` FROM outgoing_transfers WHERE idempotency_key=$1`, r.Key))
		if e != nil {
			return Transfer{}, e
		}
		if t.AccountID != r.AccountID || t.Destination != r.Destination || t.Amount != r.Amount || !sameID(t.BeneficiaryID, r.BeneficiaryID) {
			return Transfer{}, ledger.ErrConflict
		}
		return t, tx.Commit(ctx)
	}
	if err != nil {
		return Transfer{}, err
	}
	_, available, reserved, err := backing(ctx, tx, r.AccountID)
	if err != nil {
		return Transfer{}, err
	}
	res, err := ledger.PostTx(ctx, tx, ledger.Posting{Key: fmt.Sprintf("bank:reserve:%d", id), Description: "Reserve outgoing transfer", Entries: []ledger.Entry{{AccountID: available, Side: "debit", Amount: r.Amount}, {AccountID: reserved, Side: "credit", Amount: r.Amount}}})
	if err != nil {
		return Transfer{}, err
	}
	_, err = tx.Exec(ctx, `UPDATE outgoing_transfers SET reserve_journal_id=$2 WHERE id=$1`, id, res.ID)
	if err != nil {
		return Transfer{}, err
	}
	t, err := scanTransfer(tx.QueryRow(ctx, `SELECT `+transferColumns+` FROM outgoing_transfers WHERE id=$1`, id))
	if err != nil {
		return Transfer{}, err
	}
	return t, tx.Commit(ctx)
}
func (s *Store) Transfer(ctx context.Context, id int64) (Transfer, error) {
	return scanTransfer(s.DB.QueryRow(ctx, `SELECT `+transferColumns+` FROM outgoing_transfers WHERE id=$1`, id))
}

// ProcessOne claims a durable pending transfer. Multiple workers skip locked rows.
// The simulator's receipt commits on another connection before local settlement.
// If settlement rolls back, retry discovers that receipt instead of paying twice.
func (s *Store) ProcessOne(ctx context.Context) (bool, error) {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer rollback(tx)
	t, err := scanTransfer(tx.QueryRow(ctx, `SELECT `+transferColumns+` FROM outgoing_transfers WHERE status='pending' ORDER BY id FOR UPDATE SKIP LOCKED LIMIT 1`))
	if errors.Is(err, ledger.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err = s.accept(ctx, t); err != nil {
		return false, err
	}
	cash, _, reserved, err := backing(ctx, tx, t.AccountID)
	if err != nil {
		return false, err
	}
	res, err := ledger.PostTx(ctx, tx, ledger.Posting{Key: fmt.Sprintf("bank:settle:%d", t.ID), Description: "Settle outgoing transfer", Entries: []ledger.Entry{{AccountID: reserved, Side: "debit", Amount: t.Amount}, {AccountID: cash, Side: "credit", Amount: t.Amount}}})
	if err != nil {
		return false, err
	}
	_, err = tx.Exec(ctx, `UPDATE outgoing_transfers SET status='settled',settlement_journal_id=$2,settled_at=clock_timestamp() WHERE id=$1`, t.ID, res.ID)
	if err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}
func (s *Store) accept(ctx context.Context, t Transfer) error {
	_, err := s.DB.Exec(ctx, `INSERT INTO simulated_bank_receipts(transfer_id,destination,amount) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, t.ID, t.Destination, t.Amount)
	if err != nil {
		return err
	}
	var destination string
	var amount int64
	err = s.DB.QueryRow(ctx, `SELECT destination,amount FROM simulated_bank_receipts WHERE transfer_id=$1`, t.ID).Scan(&destination, &amount)
	if err == nil && (destination != t.Destination || amount != t.Amount) {
		return ledger.ErrConflict
	}
	return err
}

func fundKey(key string) string { return fmt.Sprintf("bank:fund:%x", sha256.Sum256([]byte(key))) }

func validDestination(r Request) bool {
	if r.BeneficiaryID != nil {
		return *r.BeneficiaryID > 0 && r.Destination == ""
	}
	return strings.TrimSpace(r.Destination) != "" && len(r.Destination) <= 100
}
func sameID(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
