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
	if err := run(); err != nil {
		slog.Error("ledger stopped", "error", err)
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
	databasePool, err := pgxpool.NewWithConfig(applicationContext, databaseConfig)
	if err != nil {
		return err
	}
	defer closePool(databasePool, settings.Shutdown/2)
	startupContext, cancelStartup := context.WithTimeout(applicationContext, 30*time.Second)
	defer cancelStartup()
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
	listener, err := net.Listen("tcp", listenAddress)
	if err != nil {
		return err
	}
	slog.Info("listening", "address", listenAddress)
	return serve(applicationContext, listener, databasePool, settings)
}
