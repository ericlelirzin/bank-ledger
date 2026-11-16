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

func (store *Store) post(requestContext context.Context, posting Posting, transfer *Transfer) (Result, error) {
	// Sorting and transfer conversion must not change the caller's request.
	posting.Entries = slices.Clone(posting.Entries)
	transaction, err := store.DB.Begin(requestContext)
	if err != nil {
		return Result{}, err
	}
	defer func() {
		// The request may already be cancelled when cleanup runs.
		cleanupContext, cancelCleanup := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancelCleanup()
		_ = transaction.Rollback(cleanupContext)
	}()

	// Hash before deriving sides or reordering entries: identity is the request.
	requestDigest := requestHash(posting, transfer)
	result, err := claimJournal(requestContext, transaction, posting, requestDigest[:])
	if err != nil {
		return result, err
	}
	if result.Replayed {
		return result, transaction.Commit(requestContext)
	}

	lockedAccounts, err := lockAccounts(requestContext, transaction, posting.Entries)
	if err != nil {
		return result, err
	}
	if transfer != nil {
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

	// Apply increases first to avoid transient overdrafts. PostgreSQL rejects
	// any intermediate balance outside the int64 range.
	increasesBalance := func(entry Entry) bool {
		return entry.Side == lockedAccounts[entry.AccountID].NormalSide
	}
	sort.SliceStable(posting.Entries, func(leftIndex, rightIndex int) bool {
		return increasesBalance(posting.Entries[leftIndex]) && !increasesBalance(posting.Entries[rightIndex])
	})
	for entryIndex, entry := range posting.Entries {
		_, err = transaction.Exec(requestContext, `
			INSERT INTO entries(journal_id,line,account_id,side,amount)
			VALUES($1,$2,$3,$4,$5)`,
			result.ID, entryIndex+1, entry.AccountID, entry.Side, entry.Amount)
		if err != nil {
			return Result{}, err
		}
	}

	// Deferred journal validation runs here. An error can also mean that the
	// COMMIT acknowledgment was lost; only a same-key retry resolves that case.
	return result, transaction.Commit(requestContext)
}

// requestHash is part of the persisted replay protocol. Keep this encoding
// stable across refactors: the operation tag, field names, and entry order count.
func requestHash(posting Posting, transfer *Transfer) [sha256.Size]byte {
	var requestJSON []byte
	// These concrete types contain only strings, integers, and slices;
	// json.Marshal cannot fail for their values.
	if transfer != nil {
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

func claimJournal(requestContext context.Context, transaction pgx.Tx, posting Posting, requestDigest []byte) (Result, error) {
	var result Result
	// The unique key waits for a concurrent holder to commit or roll back.
	err := transaction.QueryRow(requestContext, `
		INSERT INTO journals(idempotency_key,request_hash,description)
		VALUES($1,$2,$3)
		ON CONFLICT(idempotency_key) DO NOTHING
		RETURNING id`, posting.Key, requestDigest, posting.Description).Scan(&result.ID)
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

func lockAccounts(requestContext context.Context, transaction pgx.Tx, entries []Entry) (map[int64]Account, error) {
	accountIDs := []int64{}
	seenAccountIDs := map[int64]bool{}
	for _, entry := range entries {
		if !seenAccountIDs[entry.AccountID] {
			accountIDs = append(accountIDs, entry.AccountID)
			seenAccountIDs[entry.AccountID] = true
		}
	}
	// Every posting takes account locks in the same order, regardless of entry order.
	slices.Sort(accountIDs)
	lockedAccounts := map[int64]Account{}
	journalCurrency := ""
	for _, accountID := range accountIDs {
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
