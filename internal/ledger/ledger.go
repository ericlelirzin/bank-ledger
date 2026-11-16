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

type Store struct {
	// DB is the shared PostgreSQL connection pool. Each posting uses a transaction
	// acquired from this pool to keep its database operations together.
	DB *pgxpool.Pool
}

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

func scanAccount(row pgx.Row) (Account, error) {
	var account Account
	// Scan destinations follow the SQL column order in accountColumns.
	err := row.Scan(
		&account.ID, &account.Code, &account.Name, &account.Kind,
		&account.Currency, &account.NormalSide, &account.AllowOverdraft, &account.Balance,
	)
	return account, err
}

func (store *Store) CreateAccount(requestContext context.Context, account Account) (Account, error) {
	if account.Balance != 0 || account.ID != 0 {
		return Account{}, ErrInvalid
	}
	return scanAccount(store.DB.QueryRow(requestContext, `
		INSERT INTO accounts(code,name,kind,currency,normal_side,allow_overdraft)
		VALUES($1,$2,$3,$4,$5,$6)
		RETURNING `+accountColumns,
		account.Code, account.Name, account.Kind,
		account.Currency, account.NormalSide, account.AllowOverdraft,
	))
}

func (store *Store) Account(requestContext context.Context, accountID int64) (Account, error) {
	account, err := scanAccount(store.DB.QueryRow(requestContext, `SELECT `+accountColumns+` FROM accounts WHERE id=$1`, accountID))
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return account, err
}

func (store *Store) Accounts(requestContext context.Context) ([]Account, error) {
	rows, err := store.DB.Query(requestContext, `SELECT `+accountColumns+` FROM accounts ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	accounts := []Account{}
	for rows.Next() {
		account, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		accounts = append(accounts, account)
	}
	return accounts, rows.Err()
}

func Validate(posting Posting) error {
	if len(posting.Key) == 0 || len(posting.Key) > 200 || len(posting.Description) > 1000 || len(posting.Entries) < 2 || len(posting.Entries) > 1000 {
		return ErrInvalid
	}
	// Sum using arbitrary precision; individual amounts remain signed 64-bit integers.
	debitTotal, creditTotal := new(big.Int), new(big.Int)
	for _, entry := range posting.Entries {
		if entry.AccountID <= 0 || entry.Amount <= 0 || (entry.Side != "debit" && entry.Side != "credit") {
			return ErrInvalid
		}
		if entry.Side == "debit" {
			debitTotal.Add(debitTotal, big.NewInt(entry.Amount))
		} else {
			creditTotal.Add(creditTotal, big.NewInt(entry.Amount))
		}
	}
	if debitTotal.Cmp(creditTotal) != 0 {
		return ErrInvalid
	}
	return nil
}

func (store *Store) Post(requestContext context.Context, posting Posting) (Result, error) {
	if err := Validate(posting); err != nil {
		return Result{}, err
	}
	return store.post(requestContext, posting, nil)
}

func (store *Store) Transfer(requestContext context.Context, transfer Transfer) (Result, error) {
	if transfer.From <= 0 || transfer.To <= 0 || transfer.From == transfer.To || transfer.Amount <= 0 || len(transfer.Key) == 0 || len(transfer.Key) > 200 {
		return Result{}, ErrInvalid
	}
	posting := Posting{
		Key:         transfer.Key,
		Description: "internal transfer",
		Entries: []Entry{
			{AccountID: transfer.From, Amount: transfer.Amount},
			{AccountID: transfer.To, Amount: transfer.Amount},
		},
	}
	return store.post(requestContext, posting, &transfer)
}

// Journal is a committed posting and its immutable entry lines.
type Journal struct {
	ID          int64     `json:"id"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
	Entries     []Entry   `json:"entries"`
}

func (store *Store) Journal(requestContext context.Context, journalID int64) (Journal, error) {
	var journal Journal
	err := store.DB.QueryRow(requestContext, `SELECT id,description,created_at FROM journals WHERE id=$1`, journalID).Scan(&journal.ID, &journal.Description, &journal.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return journal, ErrNotFound
	}
	if err != nil {
		return journal, err
	}
	rows, err := store.DB.Query(requestContext, `SELECT account_id,side,amount FROM entries WHERE journal_id=$1 ORDER BY line`, journalID)
	if err != nil {
		return journal, err
	}
	defer rows.Close()
	journal.Entries = []Entry{}
	for rows.Next() {
		var entry Entry
		if err = rows.Scan(&entry.AccountID, &entry.Side, &entry.Amount); err != nil {
			return journal, err
		}
		journal.Entries = append(journal.Entries, entry)
	}
	return journal, rows.Err()
}
