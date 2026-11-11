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
	c, e := loadConfig()
	if e != nil {
		return e
	}
	pc, e := poolConfig(databaseURL, c)
	if e != nil {
		return e
	}
	db, e := pgxpool.NewWithConfig(ctx, pc)
	if e != nil {
		return e
	}
	defer closePool(db, c.Shutdown/2)
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
	listener, e := net.Listen("tcp", addr)
	if e != nil {
		return e
	}
	slog.Info("listening", "address", addr)
	return serve(ctx, listener, db, c)
}
