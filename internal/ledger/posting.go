// Read ledger.go for the request types, then follow post from begin to commit.
// Every helper that writes or locks receives the SAME PostgreSQL transaction.
package ledger

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// post owns the transaction boundary for both journal and transfer requests.
// The key claim, entries, and trigger-maintained balances commit or roll back
// together. Helpers never open an independent transaction for part of the work.
func (store *Store) post(requestContext context.Context, posting Posting, transfer *Transfer) (Result, error) {
	// A struct copy still shares a slice's backing array with the caller. Clone
	// before sorting so a later retry still has its original request and hash.
	posting.Entries = slices.Clone(posting.Entries)
	// Begin acquires a pool connection and starts a transaction on it. Use
	// transaction.Exec/QueryRow below; store.DB.Exec could use another connection.
	transaction, err := store.DB.Begin(requestContext)
	if err != nil {
		return Result{}, err
	}
	// defer runs on every return, covering all errors below. After a successful
	// commit Rollback is harmless: pgx reports that the transaction is closed.
	defer func() {
		// The request may already be cancelled when cleanup runs.
		cleanupContext, cancelCleanup := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancelCleanup()
		_ = transaction.Rollback(cleanupContext)
	}()

	// Hash before deriving sides or reordering entries: identity is the request.
	requestDigest := requestHash(posting, transfer)
	// Sum256 returns a fixed-size array; [:] exposes its bytes as the slice pgx
	// binds to PostgreSQL bytea. The unique key arbitrates retries before locks.
	result, err := claimJournal(requestContext, transaction, posting, requestDigest[:])
	if err != nil {
		return result, err
	}
	if result.Replayed {
		// The original transaction already committed the money movement. Finish
		// this transaction without inserting entries or moving balances again.
		return result, transaction.Commit(requestContext)
	}

	// Locks remain held until commit/rollback, not just until this helper returns.
	lockedAccounts, err := lockAccounts(requestContext, transaction, posting.Entries)
	if err != nil {
		return result, err
	}
	if transfer != nil {
		// A transfer decreases the source's normal balance and increases the
		// destination's. Matching normal sides makes these two lines balance.
		sourceAccount, destinationAccount := lockedAccounts[transfer.From], lockedAccounts[transfer.To]
		if sourceAccount.NormalSide != destinationAccount.NormalSide {
			return result, fmt.Errorf("%w: transfer accounts need the same normal side", ErrInvalid)
		}
		posting.Entries[0].Side = "credit"
		if sourceAccount.NormalSide == "credit" {
			posting.Entries[0].Side = "debit"
		}
		posting.Entries[1].Side = destinationAccount.NormalSide
	}

	// Account CHECK constraints run on each balance update. Increase first so a
	// decrease is not rejected merely because it preceded an offsetting increase.
	// Intermediate int64 overflow is still rejected, even if the final net fits.
	increasesBalance := func(entry Entry) bool {
		return entry.Side == lockedAccounts[entry.AccountID].NormalSide
	}
	sort.SliceStable(posting.Entries, func(leftIndex, rightIndex int) bool {
		// true means "left belongs before right". Stable preserves relative
		// order within the increases group and within the decreases group.
		return increasesBalance(posting.Entries[leftIndex]) && !increasesBalance(posting.Entries[rightIndex])
	})
	for entryIndex, entry := range posting.Entries {
		// Inserting a line fires apply_entry in migrations/001_ledger.sql, which
		// updates its account balance inside THIS transaction. No Go balance
		// update is needed. SQL line numbers start at 1; slice indices at 0.
		_, err = transaction.Exec(requestContext, `
			INSERT INTO entries(journal_id,line,account_id,side,amount)
			VALUES($1,$2,$3,$4,$5)`,
			result.ID, entryIndex+1, entry.AccountID, entry.Side, entry.Amount)
		if err != nil {
			// Even if earlier inserts succeeded, deferred rollback removes their
			// entries, balance effects, and the key claim together.
			return Result{}, err
		}
	}

	// Deferred journal validation runs here. An error can also mean that the
	// COMMIT acknowledgment was lost; only a same-key retry resolves that case.
	return result, transaction.Commit(requestContext)
}

// requestHash is part of the persisted replay protocol. Keep this encoding
// stable across refactors: the operation tag, field names, and entry order count.
// Hash decoded typed fields so JSON whitespace/property order do not matter.
// This digest compares request identity; it is not authentication or encryption.
func requestHash(posting Posting, transfer *Transfer) [sha256.Size]byte {
	var requestJSON []byte
	// These concrete types contain only strings, integers, and slices;
	// json.Marshal cannot fail for their values.
	if transfer != nil {
		// The operation tag distinguishes a transfer from an equivalent journal.
		// These anonymous struct fields are serialized too; renaming them would
		// change persisted hashes. TestRequestHashCompatibility guards that format.
		requestJSON, _ = json.Marshal(struct {
			Type     string
			Transfer Transfer
		}{"transfer", *transfer})
	} else {
		requestJSON, _ = json.Marshal(struct {
			Type    string
			Posting Posting
		}{"journal", posting})
	}
	return sha256.Sum256(requestJSON)
}

// claimJournal returns a newly claimed journal or an identical committed replay.
// A conflicting payload is an error. A failed transaction never reserves its key.
func claimJournal(requestContext context.Context, transaction pgx.Tx, posting Posting, requestDigest []byte) (Result, error) {
	var result Result
	// PostgreSQL's UNIQUE index coordinates every process using this database.
	// If another transaction holds the key, this insert waits: a committed holder
	// causes DO NOTHING; a rolled-back holder lets this insert claim the key.
	err := transaction.QueryRow(requestContext, `
		INSERT INTO journals(idempotency_key,request_hash,description)
		VALUES($1,$2,$3)
		ON CONFLICT(idempotency_key) DO NOTHING
		RETURNING id`, posting.Key, requestDigest, posting.Description).Scan(&result.ID)
	// RETURNING gives a row only for a new insert. ErrNoRows means DO NOTHING
	// skipped it. Other errors (including cancellation) must propagate.
	if !errors.Is(err, pgx.ErrNoRows) {
		return result, err
	}

	// At READ COMMITTED this separate statement sees the winning commit.
	var storedHash []byte
	err = transaction.QueryRow(requestContext, `SELECT id,request_hash FROM journals WHERE idempotency_key=$1`, posting.Key).Scan(&result.ID, &storedHash)
	if err != nil {
		return result, err
	}
	if !bytes.Equal(storedHash, requestDigest) {
		return Result{}, ErrConflict
	}
	result.Replayed = true
	return result, nil
}

// lockAccounts deduplicates account IDs, locks them in a common order, and checks
// one currency. The returned map supplies metadata to transfer conversion/sorting.
func lockAccounts(requestContext context.Context, transaction pgx.Tx, entries []Entry) (map[int64]Account, error) {
	accountIDs := []int64{}
	// A missing map key returns false, so repeated lines need no repeated lock.
	seenAccountIDs := map[int64]bool{}
	for _, entry := range entries {
		if !seenAccountIDs[entry.AccountID] {
			accountIDs = append(accountIDs, entry.AccountID)
			seenAccountIDs[entry.AccountID] = true
		}
	}
	// Consistent ordering avoids circular waits between application postings,
	// including transfers in opposite directions. SQL writers using another
	// order can still deadlock; PostgreSQL then aborts one transaction.
	slices.Sort(accountIDs)
	lockedAccounts := map[int64]Account{}
	journalCurrency := ""
	for _, accountID := range accountIDs {
		// FOR UPDATE coordinates across application instances. A Go mutex would
		// only coordinate goroutines in one process. Other postings touching this
		// account wait; postings using disjoint accounts can continue concurrently.
		account, err := scanAccount(transaction.QueryRow(requestContext, `SELECT `+accountColumns+` FROM accounts WHERE id=$1 FOR UPDATE`, accountID))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		if journalCurrency != "" && journalCurrency != account.Currency {
			return nil, ErrInvalid
		}
		journalCurrency = account.Currency
		lockedAccounts[accountID] = account
	}
	return lockedAccounts, nil
}
