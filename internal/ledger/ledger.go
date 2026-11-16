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
	// Callers use errors.Is to recognize these categories without parsing text.
	// The HTTP layer translates them into 400, 409, and 404 responses.
	ErrInvalid  = errors.New("invalid request")
	ErrConflict = errors.New("idempotency key reused with different request")
	ErrNotFound = errors.New("resource not found")
)

// Store groups operations that use the same database pool. The *Store receiver
// on a method gives that method access to DB without opening a new pool.
type Store struct {
	// DB is the shared PostgreSQL connection pool. Each posting uses a transaction
	// acquired from this pool to keep its database operations together.
	DB *pgxpool.Pool
}

// Account combines fixed accounting policy with the current stored balance.
// json tags define HTTP field names independently of the Go field names.
// PostgreSQL entry triggers maintain Balance; callers cannot set opening funds.
type Account struct {
	ID       int64  `json:"id"`
	Code     string `json:"code"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Currency string `json:"currency"`
	// NormalSide identifies which entry side increases this account's balance.
	NormalSide     string `json:"normal_side"`
	AllowOverdraft bool   `json:"allow_overdraft"`
	Balance        int64  `json:"balance"`
}

// Entry is one journal line. Amount is a positive int64 in currency minor units;
// Side determines its effect, so a debit is not represented as a negative amount.
type Entry struct {
	AccountID int64  `json:"account_id"`
	Side      string `json:"side"`
	Amount    int64  `json:"amount"`
}

// Posting is the request to create a journal. Key identifies this exact request
// across retries; Entries is a slice whose order participates in request identity.
type Posting struct {
	Key         string  `json:"idempotency_key"`
	Description string  `json:"description"`
	Entries     []Entry `json:"entries"`
}

// Transfer is a restricted two-account posting. The store derives entry sides
// from the locked accounts instead of trusting the client to supply them.
type Transfer struct {
	Key    string `json:"idempotency_key"`
	From   int64  `json:"from_account_id"`
	To     int64  `json:"to_account_id"`
	Amount int64  `json:"amount"`
}

// Result identifies the committed journal when the accompanying error is nil.
// Replayed means an identical request had already committed using this key.
// A nonzero ID alongside an error is not proof that the transaction committed.
type Result struct {
	ID       int64 `json:"journal_id"`
	Replayed bool  `json:"replayed"`
}

// Keep this SQL column list in the same order as the destinations in scanAccount.
const accountColumns = `id,code,name,kind,currency,normal_side,allow_overdraft,balance`

// scanAccount is the SQL-to-Go boundary for a single account row. Passing &field
// gives Scan an address to write into. QueryRow execution errors surface at Scan.
func scanAccount(row pgx.Row) (Account, error) {
	var account Account
	// Scan destinations follow the SQL column order in accountColumns.
	err := row.Scan(
		&account.ID, &account.Code, &account.Name, &account.Kind,
		&account.Currency, &account.NormalSide, &account.AllowOverdraft, &account.Balance,
	)
	return account, err
}

// CreateAccount creates identity and policy, with a database-assigned ID and zero
// balance. Funding must go through a balanced posting so the ledger explains it.
func (store *Store) CreateAccount(requestContext context.Context, account Account) (Account, error) {
	if account.Balance != 0 || account.ID != 0 {
		return Account{}, ErrInvalid
	}
	// $1..$6 bind arguments as data. RETURNING reads the inserted row, including
	// database defaults, without a separate SELECT. SQL enforces account policy.
	return scanAccount(store.DB.QueryRow(requestContext, `
		INSERT INTO accounts(code,name,kind,currency,normal_side,allow_overdraft)
		VALUES($1,$2,$3,$4,$5,$6)
		RETURNING `+accountColumns,
		account.Code, account.Name, account.Kind,
		account.Currency, account.NormalSide, account.AllowOverdraft,
	))
}

// Account reads a committed balance. This unlocked read is informational: another
// posting can change it immediately, so it cannot authorize a later spend.
func (store *Store) Account(requestContext context.Context, accountID int64) (Account, error) {
	account, err := scanAccount(store.DB.QueryRow(requestContext, `SELECT `+accountColumns+` FROM accounts WHERE id=$1`, accountID))
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return account, err
}

// Accounts reads all accounts in one statement and therefore one statement
// snapshot. This educational endpoint has no pagination; result size can grow.
func (store *Store) Accounts(requestContext context.Context) ([]Account, error) {
	rows, err := store.DB.Query(requestContext, `SELECT `+accountColumns+` FROM accounts ORDER BY id`)
	if err != nil {
		return nil, err
	}
	// Release the result stream even if scanning a row fails partway through.
	defer rows.Close()
	// A non-nil empty slice encodes as [] in JSON, rather than null.
	accounts := []Account{}
	for rows.Next() {
		account, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		accounts = append(accounts, account)
	}
	// Next also stops on transport/query errors. Checking Err distinguishes that
	// failure from a complete result; otherwise a partial list could look valid.
	return accounts, rows.Err()
}

// Validate rejects malformed postings before taking a pooled connection or locks.
// It cannot check account currency or policy: those require database rows.
// PostgreSQL independently validates journal totals at commit for all SQL writers.
func Validate(posting Posting) error {
	if len(posting.Key) == 0 || len(posting.Key) > 200 || len(posting.Description) > 1000 || len(posting.Entries) < 2 || len(posting.Entries) > 1000 {
		return ErrInvalid
	}
	// Each amount fits int64, but a sum of many amounts may exceed int64. big.Int
	// avoids a wrapped total accidentally making an unbalanced request look valid.
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
	// Cmp returns zero for equal totals. == on these pointers would compare
	// addresses, not the numeric totals they hold.
	if debitTotal.Cmp(creditTotal) != 0 {
		return ErrInvalid
	}
	return nil
}

// Post validates explicit debit/credit lines, then enters the shared transaction
// protocol. A nil transfer argument means the sides are already supplied.
func (store *Store) Post(requestContext context.Context, posting Posting) (Result, error) {
	if err := Validate(posting); err != nil {
		return Result{}, err
	}
	return store.post(requestContext, posting, nil)
}

// Transfer uses the same atomic protocol as Post. Its two sides are initially
// unset: post derives them after locking and reading both accounts' normal sides.
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
	// &transfer supplies a pointer to the transfer request. Unlike nil, it tells
	// post to derive sides and hash the original transfer representation.
	return store.post(requestContext, posting, &transfer)
}

// Journal is a committed posting and its immutable entry lines.
type Journal struct {
	ID          int64     `json:"id"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
	Entries     []Entry   `json:"entries"`
}

// Journal reads header and lines separately. That is safe here because committed
// journals cannot be edited or appended to. Lines reflect insertion order, which
// can differ from the original request after increases-first sorting in post.
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
