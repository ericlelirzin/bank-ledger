package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"bank-ledger/internal/ledger"

	"github.com/jackc/pgx/v5/pgconn"
)

func Handler(s *ledger.Store) http.Handler {
	mux := http.NewServeMux()
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
	return mux
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		respond(w, nil, ledger.ErrInvalid)
		return false
	}
	if e := d.Decode(new(any)); e != io.EOF {
		respond(w, nil, ledger.ErrInvalid)
		return false
	}
	return true
}
func respond(w http.ResponseWriter, v any, err error) {
	w.Header().Set("Content-Type", "application/json")
	status := http.StatusOK
	if err != nil {
		status = 500
		message := "internal error"
		var pg *pgconn.PgError
		switch {
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
			case "40P01", "40001":
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
