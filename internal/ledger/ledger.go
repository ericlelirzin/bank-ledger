// Package ledger implements atomic, immutable double-entry posting.
package ledger

import (
	"context"
	"errors"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrInvalid  = errors.New("invalid request")
	ErrConflict = errors.New("idempotency key reused with different request")
	ErrNotFound = errors.New("resource not found")
)

type Store struct{ DB *pgxpool.Pool }
type Account struct {
	ID             int64  `json:"id"`
	Code           string `json:"code"`
	Name           string `json:"name"`
	Kind           string `json:"kind"`
	Currency       string `json:"currency"`
	NormalSide     string `json:"normal_side"`
	AllowOverdraft bool   `json:"allow_overdraft"`
	Balance        int64  `json:"balance"`
}
type Entry struct {
	AccountID int64  `json:"account_id"`
	Side      string `json:"side"`
	Amount    int64  `json:"amount"`
}
type Posting struct {
	Key         string  `json:"idempotency_key"`
	Description string  `json:"description"`
	Entries     []Entry `json:"entries"`
}
type Transfer struct {
	Key    string `json:"idempotency_key"`
	From   int64  `json:"from_account_id"`
	To     int64  `json:"to_account_id"`
	Amount int64  `json:"amount"`
}
type Result struct {
	ID       int64 `json:"journal_id"`
	Replayed bool  `json:"replayed"`
}

const accountColumns = `id,code,name,kind,currency,normal_side,allow_overdraft,balance`

func scanAccount(row pgx.Row) (a Account, err error) {
	err = row.Scan(&a.ID, &a.Code, &a.Name, &a.Kind, &a.Currency, &a.NormalSide, &a.AllowOverdraft, &a.Balance)
	return
}
func (s *Store) CreateAccount(ctx context.Context, a Account) (Account, error) {
	if a.Balance != 0 || a.ID != 0 {
		return Account{}, ErrInvalid
	}
	return scanAccount(s.DB.QueryRow(ctx, `INSERT INTO accounts(code,name,kind,currency,normal_side,allow_overdraft) VALUES($1,$2,$3,$4,$5,$6) RETURNING `+accountColumns, a.Code, a.Name, a.Kind, a.Currency, a.NormalSide, a.AllowOverdraft))
}
func (s *Store) Account(ctx context.Context, id int64) (Account, error) {
	a, e := scanAccount(s.DB.QueryRow(ctx, `SELECT `+accountColumns+` FROM accounts WHERE id=$1`, id))
	if errors.Is(e, pgx.ErrNoRows) {
		e = ErrNotFound
	}
	return a, e
}
func (s *Store) Accounts(ctx context.Context) ([]Account, error) {
	rows, e := s.DB.Query(ctx, `SELECT `+accountColumns+` FROM accounts ORDER BY id`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Account{}
	for rows.Next() {
		a, e := scanAccount(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
func Validate(p Posting) error {
	if len(p.Key) == 0 || len(p.Key) > 200 || len(p.Description) > 1000 || len(p.Entries) < 2 || len(p.Entries) > 1000 {
		return ErrInvalid
	}
	// Sum using arbitrary precision; individual amounts remain signed 64-bit integers.
	debit, credit := new(big.Int), new(big.Int)
	for _, e := range p.Entries {
		if e.AccountID <= 0 || e.Amount <= 0 || (e.Side != "debit" && e.Side != "credit") {
			return ErrInvalid
		}
		if e.Side == "debit" {
			debit.Add(debit, big.NewInt(e.Amount))
		} else {
			credit.Add(credit, big.NewInt(e.Amount))
		}
	}
	if debit.Cmp(credit) != 0 {
		return ErrInvalid
	}
	return nil
}
func (s *Store) Post(ctx context.Context, p Posting) (Result, error) {
	if e := Validate(p); e != nil {
		return Result{}, e
	}
	return s.post(ctx, p, nil)
}
func (s *Store) Transfer(ctx context.Context, t Transfer) (Result, error) {
	if t.From <= 0 || t.To <= 0 || t.From == t.To || t.Amount <= 0 || len(t.Key) == 0 || len(t.Key) > 200 {
		return Result{}, ErrInvalid
	}
	p := Posting{Key: t.Key, Description: "internal transfer", Entries: []Entry{{AccountID: t.From, Amount: t.Amount}, {AccountID: t.To, Amount: t.Amount}}}
	return s.post(ctx, p, &t)
}

// Journal is a committed posting and its immutable entry lines.
type Journal struct {
	ID          int64     `json:"id"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
	Entries     []Entry   `json:"entries"`
}

func (s *Store) Journal(ctx context.Context, id int64) (Journal, error) {
	var j Journal
	err := s.DB.QueryRow(ctx, `SELECT id,description,created_at FROM journals WHERE id=$1`, id).Scan(&j.ID, &j.Description, &j.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return j, ErrNotFound
	}
	if err != nil {
		return j, err
	}
	rows, err := s.DB.Query(ctx, `SELECT account_id,side,amount FROM entries WHERE journal_id=$1 ORDER BY line`, id)
	if err != nil {
		return j, err
	}
	defer rows.Close()
	j.Entries = []Entry{}
	for rows.Next() {
		var e Entry
		if err = rows.Scan(&e.AccountID, &e.Side, &e.Amount); err != nil {
			return j, err
		}
		j.Entries = append(j.Entries, e)
	}
	return j, rows.Err()
}
