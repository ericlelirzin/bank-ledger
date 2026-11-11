package httpapi

import (
	"net/http/httptest"
	"strings"
	"testing"
)

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
