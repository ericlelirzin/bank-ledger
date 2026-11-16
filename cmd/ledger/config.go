package main

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// config collects separate budgets: a whole request, individual SQL waits/work,
// an idle open transaction, and process shutdown. They protect different resources.
type config struct {
	Request, Lock, Statement, IdleTransaction, Shutdown time.Duration
}

// loadConfig starts with bounded defaults and accepts environment overrides.
// Invalid limits stop startup so the server never silently runs without bounds.
func loadConfig() (config, error) {
	settings := config{
		Request:         10 * time.Second,
		Lock:            2 * time.Second,
		Statement:       5 * time.Second,
		IdleTransaction: 5 * time.Second,
		Shutdown:        10 * time.Second,
	}
	// Each target is a pointer to a duration field. One parsing loop can assign
	// every setting without duplicating validation for each environment variable.
	for _, setting := range []struct {
		name   string
		target *time.Duration
	}{
		{"REQUEST_TIMEOUT", &settings.Request},
		{"DB_LOCK_TIMEOUT", &settings.Lock},
		{"DB_STATEMENT_TIMEOUT", &settings.Statement},
		{"DB_IDLE_TX_TIMEOUT", &settings.IdleTransaction},
		{"SHUTDOWN_TIMEOUT", &settings.Shutdown},
	} {
		if configuredValue := os.Getenv(setting.name); configuredValue != "" {
			duration, err := time.ParseDuration(configuredValue)
			if err != nil || duration < time.Millisecond || duration > time.Hour {
				return settings, fmt.Errorf("%s must be between 1ms and 1h", setting.name)
			}
			// * writes through the pointer into the corresponding settings field.
			*setting.target = duration
		}
	}
	// Prefer a database lock error before the statement budget expires, and a
	// database statement error before the whole-request budget expires.
	if settings.Lock >= settings.Statement || settings.Statement >= settings.Request {
		return settings, fmt.Errorf("timeouts must satisfy DB_LOCK_TIMEOUT < DB_STATEMENT_TIMEOUT < REQUEST_TIMEOUT")
	}
	return settings, nil
}

// poolConfig configures every connection the pool opens, including replacements.
// PostgreSQL-side limits still apply when a client cannot deliver cancellation.
func poolConfig(databaseURL string, settings config) (*pgxpool.Config, error) {
	databaseConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, err
	}
	databaseConfig.ConnConfig.ConnectTimeout = settings.Request
	for parameterName, duration := range map[string]time.Duration{
		"lock_timeout":                        settings.Lock,
		"statement_timeout":                   settings.Statement,
		"idle_in_transaction_session_timeout": settings.IdleTransaction,
	} {
		// PostgreSQL accepts these bare timeout values in milliseconds. Its
		// connection parameters are strings, so convert the Go duration explicitly.
		databaseConfig.ConnConfig.RuntimeParams[parameterName] = strconv.FormatInt(duration.Milliseconds(), 10)
	}
	return databaseConfig, nil
}
