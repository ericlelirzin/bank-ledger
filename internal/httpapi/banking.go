package httpapi

import (
	"bank-ledger/internal/banking"
	"bank-ledger/internal/ledger"
	"net/http"
	"strconv"
)

func bankRoutes(mux *http.ServeMux, s *banking.Store) {
	id := func(w http.ResponseWriter, r *http.Request) (int64, bool) {
		v, e := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if e != nil || v <= 0 {
			respond(w, nil, ledger.ErrInvalid)
			return 0, false
		}
		return v, true
	}
	page := func(w http.ResponseWriter, r *http.Request) (int64, int, bool) {
		before, limit := int64(0), 50
		q := r.URL.Query()
		if values, ok := q["before_id"]; ok {
			v, e := strconv.ParseInt(q.Get("before_id"), 10, 64)
			if len(values) != 1 || e != nil || v <= 0 {
				respond(w, nil, ledger.ErrInvalid)
				return 0, 0, false
			}
			before = v
		}
		if values, ok := q["limit"]; ok {
			v, e := strconv.Atoi(q.Get("limit"))
			if len(values) != 1 || e != nil || v < 1 || v > 100 {
				respond(w, nil, ledger.ErrInvalid)
				return 0, 0, false
			}
			limit = v
		}
		return before, limit, true
	}
	mux.HandleFunc("POST /bank/accounts/{id}/beneficiaries", func(w http.ResponseWriter, r *http.Request) {
		v, ok := id(w, r)
		if !ok {
			return
		}
		var body banking.BeneficiaryRequest
		if !decode(w, r, &body) {
			return
		}
		b, e := s.CreateBeneficiary(r.Context(), v, body)
		respond(w, b, e)
	})
	mux.HandleFunc("GET /bank/accounts/{id}/beneficiaries", func(w http.ResponseWriter, r *http.Request) {
		v, ok := id(w, r)
		if !ok {
			return
		}
		before, limit, ok := page(w, r)
		if !ok {
			return
		}
		b, e := s.Beneficiaries(r.Context(), v, before, limit)
		respond(w, b, e)
	})
	mux.HandleFunc("GET /bank/accounts/{id}/transfers", func(w http.ResponseWriter, r *http.Request) {
		v, ok := id(w, r)
		if !ok {
			return
		}
		before, limit, ok := page(w, r)
		if !ok {
			return
		}
		b, e := s.Transfers(r.Context(), v, before, limit)
		respond(w, b, e)
	})
	mux.HandleFunc("GET /bank/transfers/{id}/timeline", func(w http.ResponseWriter, r *http.Request) {
		v, ok := id(w, r)
		if !ok {
			return
		}
		b, e := s.Timeline(r.Context(), v)
		respond(w, b, e)
	})
	mux.HandleFunc("POST /bank/accounts", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		if !decode(w, r, &body) {
			return
		}
		v, e := s.Open(r.Context(), body.Name)
		respond(w, v, e)
	})
	mux.HandleFunc("GET /bank/accounts/{id}", func(w http.ResponseWriter, r *http.Request) {
		v, ok := id(w, r)
		if !ok {
			return
		}
		a, e := s.Account(r.Context(), v)
		respond(w, a, e)
	})
	mux.HandleFunc("POST /bank/accounts/{id}/simulated-funding", func(w http.ResponseWriter, r *http.Request) {
		v, ok := id(w, r)
		if !ok {
			return
		}
		var body struct {
			Key    string `json:"idempotency_key"`
			Amount int64  `json:"amount"`
		}
		if !decode(w, r, &body) {
			return
		}
		a, e := s.Fund(r.Context(), v, body.Amount, body.Key)
		respond(w, a, e)
	})
	mux.HandleFunc("POST /bank/transfers", func(w http.ResponseWriter, r *http.Request) {
		var body banking.Request
		if !decode(w, r, &body) {
			return
		}
		a, e := s.CreateTransfer(r.Context(), body)
		respond(w, a, e)
	})
	mux.HandleFunc("GET /bank/transfers/{id}", func(w http.ResponseWriter, r *http.Request) {
		v, ok := id(w, r)
		if !ok {
			return
		}
		a, e := s.Transfer(r.Context(), v)
		respond(w, a, e)
	})
}
