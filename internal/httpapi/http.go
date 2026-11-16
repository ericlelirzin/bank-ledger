// Package httpapi translates HTTP/JSON into ledger calls and maps errors back to
// HTTP. Transaction ownership and accounting rules stay in ledger/PostgreSQL.
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"bank-ledger/internal/ledger"

	"github.com/jackc/pgx/v5/pgconn"
)

// Handler builds the router once. Each registered function runs for a matching
// request; net/http can run many of these functions concurrently.
// A responseWriter sends the response, while request holds input and cancellation.
func Handler(store *ledger.Store, requestTimeout time.Duration) http.Handler {
	router := http.NewServeMux()
	router.HandleFunc("GET /healthz", func(responseWriter http.ResponseWriter, request *http.Request) {
		// Ping checks database reachability, not just whether the Go process lives.
		if err := store.DB.Ping(request.Context()); err != nil {
			respond(responseWriter, nil, err)
			return
		}
		respond(responseWriter, map[string]string{"status": "ok"}, nil)
	})
	router.HandleFunc("GET /accounts", func(responseWriter http.ResponseWriter, request *http.Request) {
		accounts, err := store.Accounts(request.Context())
		respond(responseWriter, accounts, err)
	})
	router.HandleFunc("POST /accounts", func(responseWriter http.ResponseWriter, request *http.Request) {
		// &account lets decode populate the local struct. On failure decode has
		// already written an error, so return before calling the store.
		var account ledger.Account
		if !decode(responseWriter, request, &account) {
			return
		}
		result, err := store.CreateAccount(request.Context(), account)
		respond(responseWriter, result, err)
	})
	router.HandleFunc("GET /accounts/{id}", func(responseWriter http.ResponseWriter, request *http.Request) {
		// Path values are text. Parse base-10 signed 64-bit IDs before querying SQL.
		accountID, err := strconv.ParseInt(request.PathValue("id"), 10, 64)
		if err != nil || accountID <= 0 {
			respond(responseWriter, nil, ledger.ErrInvalid)
			return
		}
		account, err := store.Account(request.Context(), accountID)
		respond(responseWriter, account, err)
	})
	router.HandleFunc("GET /journals/{id}", func(responseWriter http.ResponseWriter, request *http.Request) {
		journalID, err := strconv.ParseInt(request.PathValue("id"), 10, 64)
		if err != nil || journalID <= 0 {
			respond(responseWriter, nil, ledger.ErrInvalid)
			return
		}
		journal, err := store.Journal(request.Context(), journalID)
		respond(responseWriter, journal, err)
	})
	router.HandleFunc("POST /journals", func(responseWriter http.ResponseWriter, request *http.Request) {
		// This endpoint supplies explicit lines; /transfers supplies two accounts.
		// Both ultimately enter ledger.post and share its idempotency namespace.
		var posting ledger.Posting
		if !decode(responseWriter, request, &posting) {
			return
		}
		result, err := store.Post(request.Context(), posting)
		respond(responseWriter, result, err)
	})
	router.HandleFunc("POST /transfers", func(responseWriter http.ResponseWriter, request *http.Request) {
		var transfer ledger.Transfer
		if !decode(responseWriter, request, &transfer) {
			return
		}
		result, err := store.Transfer(request.Context(), transfer)
		respond(responseWriter, result, err)
	})
	// This wrapper applies to every route. The deadline covers pool acquisition
	// and the whole database protocol, rather than resetting for each SQL call.
	// It propagates cancellation; it does not itself interrupt a blocked body read
	// or send a timeout response. Server socket timeouts also bound client I/O.
	return http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		requestContext, cancelRequest := context.WithTimeout(request.Context(), requestTimeout)
		defer cancelRequest()
		router.ServeHTTP(responseWriter, request.WithContext(requestContext))
	})
}

// decode reads bounded input twice: first to reject ambiguous object keys, then
// to populate the expected Go type. A false result means an error was sent.
func decode(responseWriter http.ResponseWriter, request *http.Request, destination any) bool {
	// 1<<20 is 1 MiB. Bound memory use before buffering the body for both passes.
	request.Body = http.MaxBytesReader(responseWriter, request.Body, 1<<20)
	body, err := io.ReadAll(request.Body)
	if err != nil {
		respond(responseWriter, nil, ledger.ErrInvalid)
		return false
	}
	keyDecoder := json.NewDecoder(bytes.NewReader(body))
	// The token pass needs structure, not floating-point conversion of amounts.
	keyDecoder.UseNumber()
	if err = uniqueKeys(keyDecoder, 0); err != nil {
		respond(responseWriter, nil, ledger.ErrInvalid)
		return false
	}
	// Exactly one JSON value is allowed: trailing whitespace is fine, another
	// document is not. EOF is the expected success condition at this point.
	if _, err = keyDecoder.Token(); err != io.EOF {
		respond(responseWriter, nil, ledger.ErrInvalid)
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	// Reject misspelled fields rather than silently discarding them. Decoding
	// into int64 fields also rejects fractions and out-of-range integer amounts.
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(destination); err != nil {
		respond(responseWriter, nil, ledger.ErrInvalid)
		return false
	}

	return true
}

// Walk tokens before typed decoding. Fold case as encoding/json also matches
// struct fields case-insensitively; escaped spellings decode to the same key.
func uniqueKeys(decoder *json.Decoder, depth int) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	// A type assertion tests whether the token opens a container. Primitive
	// values have no keys or children to walk, so this recursive call is done.
	if !ok {
		return nil
	}
	// Match encoding/json's container-depth limit; the token walk must not
	// introduce unbounded recursion before the typed decoder applies its limit.
	if depth >= 10000 {
		return fmt.Errorf("JSON nesting limit exceeded")
	}
	switch delimiter {
	case '{':
		// Each object gets its own set: equal keys in separate objects are valid.
		seenKeys := map[string]bool{}
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			keyName, ok := key.(string)
			if !ok {
				return fmt.Errorf("invalid key")
			}
			// encoding/json accepts case aliases. Fold Unicode case cycles to one
			// representative so aliases and escaped equivalents cannot bypass the
			// duplicate check (for example side and the long-s spelling of side).
			keyName = strings.Map(func(character rune) rune {
				for {
					foldedCharacter := unicode.SimpleFold(character)
					if foldedCharacter <= character {
						return foldedCharacter
					}
					character = foldedCharacter
				}
			}, keyName)
			if seenKeys[keyName] {
				return fmt.Errorf("duplicate key")
			}
			seenKeys[keyName] = true
			if err = uniqueKeys(decoder, depth+1); err != nil {
				return err
			}
		}
	case '[':
		// Recurse into each array element so nested entry objects are checked too.
		for decoder.More() {
			if err = uniqueKeys(decoder, depth+1); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unexpected delimiter")
	}
	// Consume the closing delimiter; malformed nesting is reported by Decoder.
	_, err = decoder.Token()
	return err
}

// respond centralizes the public error contract. errors.Is matches sentinel or
// wrapped errors; errors.As extracts structured PostgreSQL error details.
func respond(responseWriter http.ResponseWriter, responseBody any, err error) {
	responseWriter.Header().Set("Content-Type", "application/json")
	statusCode := http.StatusOK
	if err != nil {
		// Start with a generic failure so unknown database errors are not exposed.
		statusCode = 500
		errorMessage := "internal error"
		var databaseError *pgconn.PgError
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			// A deadline says the caller stopped waiting, not that COMMIT failed.
			// Same-key retry is required whenever the posting outcome is uncertain.
			statusCode = 504
			errorMessage = "request deadline exceeded; posting outcome may be unknown; retry the identical request with the same idempotency key"
		case errors.Is(err, context.Canceled):
			statusCode = 503
			errorMessage = "request cancelled; posting outcome may be unknown; retry the identical request with the same idempotency key"
		case errors.Is(err, ledger.ErrInvalid):
			statusCode = 400
			errorMessage = err.Error()
		case errors.Is(err, ledger.ErrNotFound):
			statusCode = 404
			errorMessage = err.Error()
		case errors.Is(err, ledger.ErrConflict):
			statusCode = 409
			errorMessage = err.Error()
		case errors.As(err, &databaseError):
			// SQLSTATE codes are stable categories; avoid matching database prose.
			switch databaseError.Code {
			case "23505":
				statusCode = 409
				errorMessage = "account code already exists"
			case "23514", "22003":
				// CHECK violation or numeric overflow: these are rejected postings.
				statusCode = 422
				errorMessage = "posting violates account policy, balance bounds, or journal constraints"
			case "23502", "22001", "22P02":
				statusCode = 400
				errorMessage = "invalid account or posting"
			case "40P01", "40001", "55P03", "57014", "25P03":
				// Deadlock, serialization failure, lock timeout, statement cancellation,
				// or idle transaction expiry: a caller may retry with the same key.
				statusCode = 503
				errorMessage = "transaction aborted; retry with the same idempotency key"
			}
		}
		if statusCode == 500 {
			slog.Error("request failed", "error", err)
		}
		responseBody = map[string]string{"error": errorMessage}
	}
	// Headers/status must be written before the body. If sending JSON fails after
	// a commit, delivery failure cannot undo that commit; retry still uses its key.
	responseWriter.WriteHeader(statusCode)
	_ = json.NewEncoder(responseWriter).Encode(responseBody)
}
