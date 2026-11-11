package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"bank-ledger/internal/ledger"
	"bank-ledger/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresHTTPFlow(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; PostgreSQL HTTP integration test skipped")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("ledger_http_test_%d", time.Now().UnixNano())
	if _, err = admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`); err != nil {
			t.Error(err)
		}
	}()
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = migrations.Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	handler := Handler(&ledger.Store{DB: db})
	request := func(method, path, body string, status int) []byte {
		t.Helper()
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
		handler.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("%s %s: status %d, want %d; %s", method, path, w.Code, status, w.Body.String())
		}
		if w.Header().Get("Content-Type") != "application/json" {
			t.Fatal("missing JSON content type")
		}
		return w.Body.Bytes()
	}
	request("GET", "/healthz", "", 200)
	for _, body := range []string{
		`{"code":"1000","name":"Cash","kind":"asset","currency":"USD","normal_side":"debit"}`,
		`{"code":"2001","name":"Alice","kind":"liability","currency":"USD","normal_side":"credit"}`,
		`{"code":"2002","name":"Bob","kind":"liability","currency":"USD","normal_side":"credit"}`,
	} {
		request("POST", "/accounts", body, 200)
	}
	request("POST", "/accounts", `{"code":"1000","name":"Duplicate","kind":"asset","currency":"USD","normal_side":"debit"}`, 409)
	deposit := `{"idempotency_key":"deposit","description":"Opening","entries":[{"account_id":1,"side":"debit","amount":10000},{"account_id":2,"side":"credit","amount":10000}]}`
	request("POST", "/journals", deposit, 200)
	transfer := `{"idempotency_key":"transfer","from_account_id":2,"to_account_id":3,"amount":2500}`
	var first, replay ledger.Result
	if err = json.Unmarshal(request("POST", "/transfers", transfer, 200), &first); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(request("POST", "/transfers", transfer, 200), &replay); err != nil {
		t.Fatal(err)
	}
	if first.ID != replay.ID || !replay.Replayed || first.Replayed {
		t.Fatalf("replay: %+v %+v", first, replay)
	}
	request("POST", "/transfers", strings.Replace(transfer, "2500", "2600", 1), 409)
	request("POST", "/transfers", `{"idempotency_key":"too-much","from_account_id":2,"to_account_id":3,"amount":7501}`, 422)
	request("POST", "/transfers", `{"idempotency_key":"missing","from_account_id":2,"to_account_id":999,"amount":1}`, 404)
	request("POST", "/transfers", `{"idempotency_key":"self","from_account_id":2,"to_account_id":2,"amount":1}`, 400)
	request("POST", "/journals", `{"idempotency_key":"bad","entries":[{"account_id":1,"side":"debit","amount":1},{"account_id":2,"side":"credit","amount":2}]}`, 400)
	for id, want := range map[int64]int64{1: 10000, 2: 7500, 3: 2500} {
		var a ledger.Account
		if err = json.Unmarshal(request("GET", fmt.Sprint("/accounts/", id), "", 200), &a); err != nil || a.Balance != want {
			t.Fatalf("account %d: %+v %v", id, a, err)
		}
	}
	var accounts []ledger.Account
	if err = json.Unmarshal(request("GET", "/accounts", "", 200), &accounts); err != nil || len(accounts) != 3 {
		t.Fatalf("chart: %+v %v", accounts, err)
	}
	var journal ledger.Journal
	if err = json.Unmarshal(request("GET", fmt.Sprint("/journals/", first.ID), "", 200), &journal); err != nil || len(journal.Entries) != 2 {
		t.Fatalf("journal: %+v %v", journal, err)
	}
	request("GET", "/accounts/999", "", 404)
	request("GET", "/journals/999", "", 404)
	request("GET", "/accounts/not-an-id", "", 400)
	request(http.MethodPost, "/transfers", `{"amount":1.5}`, 400)
}
