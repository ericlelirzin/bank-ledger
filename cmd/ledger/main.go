// The executable wires configuration, PostgreSQL, migrations, and HTTP together.
// Start with run to follow process startup; posting logic lives in internal/ledger.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"bank-ledger/migrations"

	"github.com/jackc/pgx/v5/pgxpool"
)

// main owns the process exit code. run returns errors so its deferred cleanup
// executes before os.Exit, which would otherwise skip deferred functions.
func main() {
	if err := run(); err != nil {
		slog.Error("ledger stopped", "error", err)
		os.Exit(1)
	}
}

// run accepts either server mode or the explicit migration-only command.
func run() error {
	if len(os.Args) > 2 || (len(os.Args) == 2 && os.Args[1] != "migrate") {
		return fmt.Errorf("usage: ledger [migrate]")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	// SIGINT/SIGTERM cancel applicationContext. serve interprets cancellation as
	// a request to drain HTTP work before the database pool is closed.
	applicationContext, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	settings, err := loadConfig()
	if err != nil {
		return err
	}
	databaseConfig, err := poolConfig(databaseURL, settings)
	if err != nil {
		return err
	}
	// Build one shared pool, not a new pool per HTTP request. Connection creation
	// is lazy; successful construction alone does not prove database reachability.
	databasePool, err := pgxpool.NewWithConfig(applicationContext, databaseConfig)
	if err != nil {
		return err
	}
	defer closePool(databasePool, settings.Shutdown/2)
	startupContext, cancelStartup := context.WithTimeout(applicationContext, 30*time.Second)
	defer cancelStartup()
	// Apply the schema before accepting requests. A migration failure stops
	// startup instead of serving traffic against an unexpected schema version.
	if err = migrations.Apply(startupContext, databasePool); err != nil {
		return err
	}
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		return nil
	}
	listenAddress := os.Getenv("HTTP_ADDR")
	if listenAddress == "" {
		listenAddress = ":8080"
	}
	// Bind synchronously so address/port errors return through normal cleanup.
	listener, err := net.Listen("tcp", listenAddress)
	if err != nil {
		return err
	}
	slog.Info("listening", "address", listenAddress)
	return serve(applicationContext, listener, databasePool, settings)
}
