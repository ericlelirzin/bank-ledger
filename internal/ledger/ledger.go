// Package ledger implements atomic, immutable double-entry posting.
package ledger

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
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
func (s *Store) post(ctx context.Context, p Posting, t *Transfer) (res Result, err error) {
	p.Entries = append([]Entry(nil), p.Entries...)
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return res, err
	}
	defer tx.Rollback(context.Background())
	var payload []byte
	if t != nil {
		payload, _ = json.Marshal(struct {
			Type     string
			Transfer Transfer
		}{"transfer", *t})
	} else {
		payload, _ = json.Marshal(struct {
			Type    string
			Posting Posting
		}{"journal", p})
	}
	hash := sha256.Sum256(payload)
	err = tx.QueryRow(ctx, `INSERT INTO journals(idempotency_key,request_hash,description) VALUES($1,$2,$3) ON CONFLICT(idempotency_key) DO NOTHING RETURNING id`, p.Key, hash[:], p.Description).Scan(&res.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		var old []byte
		err = tx.QueryRow(ctx, `SELECT id,request_hash FROM journals WHERE idempotency_key=$1`, p.Key).Scan(&res.ID, &old)
		if err != nil {
			return res, err
		}
		if string(old) != string(hash[:]) {
			return Result{}, ErrConflict
		}
		res.Replayed = true
		return res, tx.Commit(ctx)
	}
	if err != nil {
		return res, err
	}
	ids := []int64{}
	seen := map[int64]bool{}
	for _, e := range p.Entries {
		if !seen[e.AccountID] {
			ids = append(ids, e.AccountID)
			seen[e.AccountID] = true
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	accounts := map[int64]Account{}
	currency := ""
	for _, id := range ids {
		a, e := scanAccount(tx.QueryRow(ctx, `SELECT `+accountColumns+` FROM accounts WHERE id=$1 FOR UPDATE`, id))
		if errors.Is(e, pgx.ErrNoRows) {
			return res, ErrNotFound
		}
		if e != nil {
			return res, e
		}
		if currency != "" && currency != a.Currency {
			return res, ErrInvalid
		}
		currency = a.Currency
		accounts[id] = a
	}
	if t != nil {
		a, b := accounts[t.From], accounts[t.To]
		if a.NormalSide != b.NormalSide {
			return res, fmt.Errorf("%w: transfer accounts need the same normal side", ErrInvalid)
		}
		p.Entries[0].Side = "credit"
		if a.NormalSide == "credit" {
			p.Entries[0].Side = "debit"
		}
		p.Entries[1].Side = b.NormalSide
	}
	// Apply increases first to avoid transient overdrafts. PostgreSQL rejects
	// any intermediate balance outside the int64 range.
	sort.SliceStable(p.Entries, func(i, j int) bool {
		return p.Entries[i].Side == accounts[p.Entries[i].AccountID].NormalSide && p.Entries[j].Side != accounts[p.Entries[j].AccountID].NormalSide
	})
	for i, e := range p.Entries {
		_, err = tx.Exec(ctx, `INSERT INTO entries(journal_id,line,account_id,side,amount) VALUES($1,$2,$3,$4,$5)`, res.ID, i+1, e.AccountID, e.Side, e.Amount)
		if err != nil {
			return Result{}, err
		}
	}
	err = tx.Commit(ctx)
	return res, err
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
