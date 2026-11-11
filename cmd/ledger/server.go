package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"bank-ledger/internal/httpapi"
	"bank-ledger/internal/ledger"
	"github.com/jackc/pgx/v5/pgxpool"
)

func serve(ctx context.Context, listener net.Listener, db *pgxpool.Pool, c config) error {
	requests, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()
	server := &http.Server{Handler: httpapi.Handler(&ledger.Store{DB: db}, c.Request), BaseContext: func(net.Listener) context.Context { return requests }, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: c.Request, WriteTimeout: c.Request + 2*time.Second, IdleTimeout: 60 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	// Reserve half the shutdown budget for cancellation and cleanup after grace.
	grace, cancel := context.WithTimeout(context.Background(), c.Shutdown/2)
	defer cancel()
	err := server.Shutdown(grace)
	cancelRequests()
	if err != nil {
		_ = server.Close()
	}

	return nil
}

// pgxpool.Close waits for checked-out connections. Bound that wait even if a
// handler or network peer fails to react to cancellation; main can still exit.
func closePool(db *pgxpool.Pool, timeout time.Duration) {
	done := make(chan struct{})
	go func() { db.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		slog.Error("database pool cleanup deadline exceeded")
	}
}
