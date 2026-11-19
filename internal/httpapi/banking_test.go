package httpapi

import (
	"bank-ledger/internal/ledger"
	"net/http/httptest"
	"testing"
	"time"
)

func TestBankHistoryRejectsInvalidPagination(t *testing.T) {
	handler := Handler(&ledger.Store{}, time.Second)
	for _, path := range []string{
		"/bank/accounts/0/transfers",
		"/bank/accounts/1/transfers?limit=0",
		"/bank/accounts/1/transfers?limit=101",
		"/bank/accounts/1/transfers?limit=1&limit=2",
		"/bank/accounts/1/transfers?before_id=-1",
		"/bank/accounts/1/transfers?before_id=0",
		"/bank/accounts/1/beneficiaries?before_id=nope",
		"/bank/accounts/1/beneficiaries?limit=",
		"/bank/transfers/nope/timeline",
	} {
		t.Run(path, func(t *testing.T) {
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
			if w.Code != 400 {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}
