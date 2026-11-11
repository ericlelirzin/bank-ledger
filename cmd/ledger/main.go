package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"bank-ledger/internal/httpapi"
	"bank-ledger/internal/ledger"
	"bank-ledger/migrations"

	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	if e := run(); e != nil {
		slog.Error("ledger stopped", "error", e)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) > 2 || (len(os.Args) == 2 && os.Args[1] != "migrate") {
		return fmt.Errorf("usage: ledger [migrate]")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db, e := pgxpool.New(ctx, databaseURL)
	if e != nil {
		return e
	}
	defer db.Close()
	startup, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if e = migrations.Apply(startup, db); e != nil {
		return e
	}
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		return nil
	}
	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	server := &http.Server{Addr: addr, Handler: httpapi.Handler(&ledger.Store{DB: db}), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second}
	done := make(chan error, 1)
	go func() { slog.Info("listening", "address", addr); done <- server.ListenAndServe() }()
	select {
	case e = <-done:
		if e == http.ErrServerClosed {
			return nil
		}
		return e
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdown)
	}
}
