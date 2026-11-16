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

func Handler(store *ledger.Store, requestTimeout time.Duration) http.Handler {
	router := http.NewServeMux()
	router.HandleFunc("GET /healthz", func(responseWriter http.ResponseWriter, request *http.Request) {
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
		var account ledger.Account
		if !decode(responseWriter, request, &account) {
			return
		}
		result, err := store.CreateAccount(request.Context(), account)
		respond(responseWriter, result, err)
	})
	router.HandleFunc("GET /accounts/{id}", func(responseWriter http.ResponseWriter, request *http.Request) {
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
	return http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		requestContext, cancelRequest := context.WithTimeout(request.Context(), requestTimeout)
		defer cancelRequest()
		router.ServeHTTP(responseWriter, request.WithContext(requestContext))
	})
}
func decode(responseWriter http.ResponseWriter, request *http.Request, destination any) bool {
	request.Body = http.MaxBytesReader(responseWriter, request.Body, 1<<20)
	body, err := io.ReadAll(request.Body)
	if err != nil {
		respond(responseWriter, nil, ledger.ErrInvalid)
		return false
	}
	keyDecoder := json.NewDecoder(bytes.NewReader(body))
	keyDecoder.UseNumber()
	if err = uniqueKeys(keyDecoder, 0); err != nil {
		respond(responseWriter, nil, ledger.ErrInvalid)
		return false
	}
	if _, err = keyDecoder.Token(); err != io.EOF {
		respond(responseWriter, nil, ledger.ErrInvalid)
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
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
		for decoder.More() {
			if err = uniqueKeys(decoder, depth+1); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unexpected delimiter")
	}
	_, err = decoder.Token()
	return err
}

func respond(responseWriter http.ResponseWriter, responseBody any, err error) {
	responseWriter.Header().Set("Content-Type", "application/json")
	statusCode := http.StatusOK
	if err != nil {
		statusCode = 500
		errorMessage := "internal error"
		var databaseError *pgconn.PgError
		switch {
		case errors.Is(err, context.DeadlineExceeded):
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
			switch databaseError.Code {
			case "23505":
				statusCode = 409
				errorMessage = "account code already exists"
			case "23514", "22003":
				statusCode = 422
				errorMessage = "posting violates account policy, balance bounds, or journal constraints"
			case "23502", "22001", "22P02":
				statusCode = 400
				errorMessage = "invalid account or posting"
			case "40P01", "40001", "55P03", "57014", "25P03":
				statusCode = 503
				errorMessage = "transaction aborted; retry with the same idempotency key"
			}
		}
		if statusCode == 500 {
			slog.Error("request failed", "error", err)
		}
		responseBody = map[string]string{"error": errorMessage}
	}
	responseWriter.WriteHeader(statusCode)
	_ = json.NewEncoder(responseWriter).Encode(responseBody)
}
