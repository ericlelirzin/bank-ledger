package main

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type config struct {
	Request, Lock, Statement, IdleTransaction, Shutdown time.Duration
}

func loadConfig() (config, error) {
	c := config{Request: 10 * time.Second, Lock: 2 * time.Second, Statement: 5 * time.Second, IdleTransaction: 5 * time.Second, Shutdown: 10 * time.Second}
	for _, v := range []struct {
		name   string
		target *time.Duration
	}{
		{"REQUEST_TIMEOUT", &c.Request}, {"DB_LOCK_TIMEOUT", &c.Lock}, {"DB_STATEMENT_TIMEOUT", &c.Statement}, {"DB_IDLE_TX_TIMEOUT", &c.IdleTransaction}, {"SHUTDOWN_TIMEOUT", &c.Shutdown},
	} {
		if raw := os.Getenv(v.name); raw != "" {
			d, e := time.ParseDuration(raw)
			if e != nil || d < time.Millisecond || d > time.Hour {
				return c, fmt.Errorf("%s must be between 1ms and 1h", v.name)
			}
			*v.target = d
		}
	}
	if c.Lock >= c.Statement || c.Statement >= c.Request {
		return c, fmt.Errorf("timeouts must satisfy DB_LOCK_TIMEOUT < DB_STATEMENT_TIMEOUT < REQUEST_TIMEOUT")
	}
	return c, nil
}
func poolConfig(url string, c config) (*pgxpool.Config, error) {
	p, e := pgxpool.ParseConfig(url)
	if e != nil {
		return nil, e
	}
	p.ConnConfig.ConnectTimeout = c.Request
	for key, d := range map[string]time.Duration{"lock_timeout": c.Lock, "statement_timeout": c.Statement, "idle_in_transaction_session_timeout": c.IdleTransaction} {
		p.ConnConfig.RuntimeParams[key] = strconv.FormatInt(d.Milliseconds(), 10)
	}
	return p, nil
}
