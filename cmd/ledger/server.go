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

func serve(applicationContext context.Context, listener net.Listener, databasePool *pgxpool.Pool, settings config) error {
	requestContext, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()
	server := &http.Server{
		Handler:           httpapi.Handler(&ledger.Store{DB: databasePool}, settings.Request),
		BaseContext:       func(net.Listener) context.Context { return requestContext },
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       settings.Request,
		WriteTimeout:      settings.Request + 2*time.Second,
		IdleTimeout:       60 * time.Second,
	}
	serverErrors := make(chan error, 1)
	go func() { serverErrors <- server.Serve(listener) }()
	select {
	case err := <-serverErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-applicationContext.Done():
	}
	// Reserve half the shutdown budget for cancellation and cleanup after grace.
	shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), settings.Shutdown/2)
	defer cancelShutdown()
	err := server.Shutdown(shutdownContext)
	cancelRequests()
	if err != nil {
		_ = server.Close()
	}

	return nil
}

// pgxpool.Close waits for checked-out connections. Bound that wait even if a
// handler or network peer fails to react to cancellation; main can still exit.
func closePool(databasePool *pgxpool.Pool, timeout time.Duration) {
	poolClosed := make(chan struct{})
	go func() { databasePool.Close(); close(poolClosed) }()
	select {
	case <-poolClosed:
	case <-time.After(timeout):
		slog.Error("database pool cleanup deadline exceeded")
	}
}
