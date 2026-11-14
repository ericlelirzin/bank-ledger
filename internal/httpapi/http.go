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

	"bank-ledger/internal/banking"
	"bank-ledger/internal/ledger"

	"github.com/jackc/pgx/v5/pgconn"
)

func Handler(s *ledger.Store, requestTimeout time.Duration) http.Handler {
	mux := http.NewServeMux()
	bankRoutes(mux, &banking.Store{DB: s.DB})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if e := s.DB.Ping(r.Context()); e != nil {
			respond(w, nil, e)
			return
		}
		respond(w, map[string]string{"status": "ok"}, nil)
	})
	mux.HandleFunc("GET /accounts", func(w http.ResponseWriter, r *http.Request) { v, e := s.Accounts(r.Context()); respond(w, v, e) })
	mux.HandleFunc("POST /accounts", func(w http.ResponseWriter, r *http.Request) {
		var a ledger.Account
		if !decode(w, r, &a) {
			return
		}
		v, e := s.CreateAccount(r.Context(), a)
		respond(w, v, e)
	})
	mux.HandleFunc("GET /accounts/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, e := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if e != nil || id <= 0 {
			respond(w, nil, ledger.ErrInvalid)
			return
		}
		v, e := s.Account(r.Context(), id)
		respond(w, v, e)
	})
	mux.HandleFunc("GET /journals/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, e := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if e != nil || id <= 0 {
			respond(w, nil, ledger.ErrInvalid)
			return
		}
		v, e := s.Journal(r.Context(), id)
		respond(w, v, e)
	})
	mux.HandleFunc("POST /journals", func(w http.ResponseWriter, r *http.Request) {
		var p ledger.Posting
		if !decode(w, r, &p) {
			return
		}
		v, e := s.Post(r.Context(), p)
		respond(w, v, e)
	})
	mux.HandleFunc("POST /transfers", func(w http.ResponseWriter, r *http.Request) {
		var t ledger.Transfer
		if !decode(w, r, &t) {
			return
		}
		v, e := s.Transfer(r.Context(), t)
		respond(w, v, e)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		mux.ServeHTTP(w, r.WithContext(ctx))
	})
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		respond(w, nil, ledger.ErrInvalid)
		return false
	}
	keys := json.NewDecoder(bytes.NewReader(body))
	keys.UseNumber()
	if err = uniqueKeys(keys, 0); err != nil {
		respond(w, nil, ledger.ErrInvalid)
		return false
	}
	if _, err = keys.Token(); err != io.EOF {
		respond(w, nil, ledger.ErrInvalid)
		return false
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if err = d.Decode(v); err != nil {
		respond(w, nil, ledger.ErrInvalid)
		return false
	}

	return true
}

// Walk tokens before typed decoding. Fold case as encoding/json also matches
// struct fields case-insensitively; escaped spellings decode to the same key.
func uniqueKeys(d *json.Decoder, depth int) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	// Match encoding/json's container-depth limit; the token walk must not
	// introduce unbounded recursion before the typed decoder applies its limit.
	if depth >= 10000 {
		return fmt.Errorf("JSON nesting limit exceeded")
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok {
				return fmt.Errorf("invalid key")
			}
			name = strings.Map(func(r rune) rune {
				for {
					next := unicode.SimpleFold(r)
					if next <= r {
						return next
					}
					r = next
				}
			}, name)
			if seen[name] {
				return fmt.Errorf("duplicate key")
			}
			seen[name] = true
			if err = uniqueKeys(d, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err = uniqueKeys(d, depth+1); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unexpected delimiter")
	}
	_, err = d.Token()
	return err
}

func respond(w http.ResponseWriter, v any, err error) {
	w.Header().Set("Content-Type", "application/json")
	status := http.StatusOK
	if err != nil {
		status = 500
		message := "internal error"
		var pg *pgconn.PgError
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			status = 504
			message = "request deadline exceeded; posting outcome may be unknown; retry the identical request with the same idempotency key"
		case errors.Is(err, context.Canceled):
			status = 503
			message = "request cancelled; posting outcome may be unknown; retry the identical request with the same idempotency key"
		case errors.Is(err, ledger.ErrInvalid):
			status = 400
			message = err.Error()
		case errors.Is(err, ledger.ErrNotFound):
			status = 404
			message = err.Error()
		case errors.Is(err, ledger.ErrConflict):
			status = 409
			message = err.Error()
		case errors.As(err, &pg):
			switch pg.Code {
			case "23505":
				status = 409
				message = "account code already exists"
			case "23514", "22003":
				status = 422
				message = "posting violates account policy, balance bounds, or journal constraints"
			case "23502", "22001", "22P02":
				status = 400
				message = "invalid account or posting"
			case "40P01", "40001", "55P03", "57014", "25P03":
				status = 503
				message = "transaction aborted; retry with the same idempotency key"
			}
		}
		if status == 500 {
			slog.Error("request failed", "error", err)
		}
		v = map[string]string{"error": message}
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
