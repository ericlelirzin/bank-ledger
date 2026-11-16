package httpapi

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"bank-ledger/internal/ledger"
)

// TestDecode rejects malformed, oversized, fractional, and trailing JSON input
// while preserving the full precision of an accepted int64 amount.
func TestDecode(t *testing.T) {
	for _, body := range []string{`{"amount":1.2}`, `{"unknown":1}`, `{"amount":1} {}`, ``, strings.Repeat(" ", (1<<20)+1) + `{"amount":1}`} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/", strings.NewReader(body))
		var v struct {
			Amount int64 `json:"amount"`
		}
		if decode(w, r, &v) || w.Code != 400 {
			t.Fatalf("accepted malformed body, status=%d", w.Code)
		}
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"amount":9223372036854775807}`))
	var v struct {
		Amount int64 `json:"amount"`
	}
	if !decode(w, r, &v) || v.Amount != 9223372036854775807 {
		t.Fatal("integer precision lost")
	}
}

// TestDuplicateKeys covers nested, escaped, and case-folded aliases. Separate
// objects may reuse a name; the same object may not.
func TestDuplicateKeys(t *testing.T) {
	for _, body := range []string{
		`{"amount":1,"amount":2}`, `{"amount":1,"Amount":2}`, `{"amount":1,"\u0061mount":2}`,
		`{"entries":[{"account_id":1,"side":"debit","side":"credit","amount":1}]}`,
		`{"entries":[{"side":"debit","ſide":"credit"}]}`,
		`{"nested":{"deeper":[{"x":1,"x":2}]}}`,
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/", strings.NewReader(body))
		var v any
		if decode(w, r, &v) || w.Code != 400 {
			t.Fatalf("accepted duplicate keys: %s", body)
		}
	}
	// Equal names in separate objects are legitimate.
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"entries":[{"amount":1},{"amount":2}]}`))
	var v any
	if !decode(w, r, &v) {
		t.Fatal("rejected separate scopes")
	}
}

// TestNestedUnknownFieldsAndDuplicateRejectionBeforePosting uses a nil pool: a
// panic would expose invalid input reaching database operations before decoding
// rejected it.
func TestNestedUnknownFieldsAndDuplicateRejectionBeforePosting(t *testing.T) {
	for _, body := range []string{
		`{"idempotency_key":"x","entries":[{"account_id":1,"side":"debit","amount":1,"unknown":0}]}`,
		`{"idempotency_key":"x","entries":[{"account_id":1,"side":"debit","side":"credit","amount":1}]}`,
	} {
		// A nil pool would panic if invalid input reached any database operation.
		h := Handler(&ledger.Store{}, time.Second)
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/journals", strings.NewReader(body))
		h.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatalf("accepted invalid posting: %d %s", w.Code, w.Body.String())
		}
	}
}

// TestJSONNestingLimit prevents the recursive token walk from accepting nesting
// beyond the typed decoder limit.
func TestJSONNestingLimit(t *testing.T) {
	body := strings.Repeat("[", 10001) + "0" + strings.Repeat("]", 10001)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/", strings.NewReader(body))
	var v any
	if decode(w, r, &v) || w.Code != 400 {
		t.Fatal("accepted excessive nesting")
	}
}
