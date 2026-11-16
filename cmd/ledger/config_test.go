package main

import (
	"testing"
	"time"
)

// TestConfig checks duration parsing, allowed bounds, timeout ordering, and
// conversion into PostgreSQL connection parameters without needing a live
// server.
func TestConfig(t *testing.T) {
	for _, key := range []string{"REQUEST_TIMEOUT", "DB_LOCK_TIMEOUT", "DB_STATEMENT_TIMEOUT", "DB_IDLE_TX_TIMEOUT", "SHUTDOWN_TIMEOUT"} {
		t.Setenv(key, "")
	}
	c, e := loadConfig()
	if e != nil {
		t.Fatal(e)
	}
	p, e := poolConfig("postgres://ledger@localhost/ledger", c)
	if e != nil {
		t.Fatal(e)
	}
	if p.ConnConfig.RuntimeParams["lock_timeout"] != "2000" || p.ConnConfig.RuntimeParams["statement_timeout"] != "5000" || p.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] != "5000" {
		t.Fatal(p.ConnConfig.RuntimeParams)
	}
	for _, raw := range []string{"0", "0s", "-1s", "500us", "2h", "invalid"} {
		t.Setenv("REQUEST_TIMEOUT", raw)
		if _, e = loadConfig(); e == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	t.Setenv("REQUEST_TIMEOUT", "1s")
	if _, e = loadConfig(); e == nil {
		t.Fatal("accepted inconsistent deadlines")
	}
	t.Setenv("REQUEST_TIMEOUT", "20s")
	c, e = loadConfig()
	if e != nil || c.Request != 20*time.Second {
		t.Fatal(c, e)
	}
}
