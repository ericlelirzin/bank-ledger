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

func (s *Store) post(ctx context.Context, p Posting, t *Transfer) (Result, error) {
	// Sorting and transfer conversion must not change the caller's request.
	p.Entries = slices.Clone(p.Entries)
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return Result{}, err
	}
	defer func() {
		// The request may already be cancelled when cleanup runs.
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()

	// Hash before deriving sides or reordering entries: identity is the request.
	hash := requestHash(p, t)
	res, err := claimJournal(ctx, tx, p, hash[:])
	if err != nil {
		return res, err
	}
	if res.Replayed {
		return res, tx.Commit(ctx)
	}

	accounts, err := lockAccounts(ctx, tx, p.Entries)
	if err != nil {
		return res, err
	}
	if t != nil {
		source, destination := accounts[t.From], accounts[t.To]
		if source.NormalSide != destination.NormalSide {
			return res, fmt.Errorf("%w: transfer accounts need the same normal side", ErrInvalid)
		}
		p.Entries[0].Side = "credit"
		if source.NormalSide == "credit" {
			p.Entries[0].Side = "debit"
		}
		p.Entries[1].Side = destination.NormalSide
	}

	// Apply increases first to avoid transient overdrafts. PostgreSQL rejects
	// any intermediate balance outside the int64 range.
	increasesBalance := func(entry Entry) bool {
		return entry.Side == accounts[entry.AccountID].NormalSide
	}
	sort.SliceStable(p.Entries, func(i, j int) bool {
		return increasesBalance(p.Entries[i]) && !increasesBalance(p.Entries[j])
	})
	for i, entry := range p.Entries {
		_, err = tx.Exec(ctx, `
			INSERT INTO entries(journal_id,line,account_id,side,amount)
			VALUES($1,$2,$3,$4,$5)`, res.ID, i+1, entry.AccountID, entry.Side, entry.Amount)
		if err != nil {
			return Result{}, err
		}
	}

	// Deferred journal validation runs here. An error can also mean that the
	// COMMIT acknowledgment was lost; only a same-key retry resolves that case.
	return res, tx.Commit(ctx)
}

// requestHash is part of the persisted replay protocol. Keep this encoding
// stable across refactors: the operation tag, field names, and entry order count.
func requestHash(p Posting, t *Transfer) [sha256.Size]byte {
	var payload []byte
	// These concrete types contain only strings, integers, and slices;
	// json.Marshal cannot fail for their values.
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
	return sha256.Sum256(payload)
}

func claimJournal(ctx context.Context, tx pgx.Tx, p Posting, hash []byte) (Result, error) {
	var res Result
	// The unique key waits for a concurrent holder to commit or roll back.
	err := tx.QueryRow(ctx, `
		INSERT INTO journals(idempotency_key,request_hash,description)
		VALUES($1,$2,$3)
		ON CONFLICT(idempotency_key) DO NOTHING
		RETURNING id`, p.Key, hash, p.Description).Scan(&res.ID)
	if !errors.Is(err, pgx.ErrNoRows) {
		return res, err
	}

	// At READ COMMITTED this separate statement sees the winning commit.
	var storedHash []byte
	err = tx.QueryRow(ctx, `SELECT id,request_hash FROM journals WHERE idempotency_key=$1`, p.Key).Scan(&res.ID, &storedHash)
	if err != nil {
		return res, err
	}
	if !bytes.Equal(storedHash, hash) {
		return Result{}, ErrConflict
	}
	res.Replayed = true
	return res, nil
}

func lockAccounts(ctx context.Context, tx pgx.Tx, entries []Entry) (map[int64]Account, error) {
	ids := []int64{}
	seen := map[int64]bool{}
	for _, entry := range entries {
		if !seen[entry.AccountID] {
			ids = append(ids, entry.AccountID)
			seen[entry.AccountID] = true
		}
	}
	// Every posting takes account locks in the same order, regardless of entry order.
	slices.Sort(ids)
	accounts := map[int64]Account{}
	currency := ""
	for _, id := range ids {
		account, err := scanAccount(tx.QueryRow(ctx, `SELECT `+accountColumns+` FROM accounts WHERE id=$1 FOR UPDATE`, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		if currency != "" && currency != account.Currency {
			return nil, ErrInvalid
		}
		currency = account.Currency
		accounts[id] = account
	}
	return accounts, nil
}
