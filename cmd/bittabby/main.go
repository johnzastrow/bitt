// Command bittabby runs the BitTabby server.
//
// DEPLOY-04: templates, stylesheets, and migrations are embedded, so this
// binary runs with no files beside it other than the database it creates.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/johnzastrow/bitt/internal/auth"
	"github.com/johnzastrow/bitt/internal/config"
	"github.com/johnzastrow/bitt/internal/ledger"
	"github.com/johnzastrow/bitt/internal/store/sqlite"
	"github.com/johnzastrow/bitt/internal/web"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		// The detail goes to stderr for the operator; nothing sensitive is
		// included, since config never logs values.
		fmt.Fprintf(os.Stderr, "bittabby: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.EnsureDataDir(); err != nil {
		return err
	}

	log.Info("starting bittabby",
		"version", version,
		"addr", cfg.Addr,
		"db", cfg.DBPath,
		"append_only_triggers", cfg.AppendOnlyTriggers,
		"secure_cookies", cfg.SecureCookies,
	)
	if !cfg.AppendOnlyTriggers {
		log.Warn("append-only ledger triggers are DISABLED -- development only (LEDGER-01)")
	}
	if !cfg.SecureCookies {
		log.Warn("secure cookie flag is DISABLED -- do not run this way over a network")
	}

	db, err := sqlite.Open(sqlite.Options{
		Path:               cfg.DBPath,
		AppendOnlyTriggers: cfg.AppendOnlyTriggers,
	})
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	// DEPLOY-03: migrations run automatically on startup.
	migrateCtx, cancelMigrate := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelMigrate()
	if err := db.Migrate(migrateCtx); err != nil {
		return err
	}
	log.Info("schema up to date")

	inst, err := db.GetInstance(context.Background())
	if err != nil {
		return err
	}
	if !inst.SetupComplete() {
		log.Info("no administrator yet -- open the app to complete first-run setup")
	}

	srv := web.New(
		cfg,
		db,
		ledger.New(db),
		auth.NewManager(db, cfg.SecureCookies),
		log,
	)

	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadTimeout:       cfg.ReadTimeout,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       2 * time.Minute,
		// Bound header size so a hostile client cannot force large allocations.
		MaxHeaderBytes: 1 << 16,
		ErrorLog:       slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	// Prune expired sessions on startup rather than on a timer: at household
	// scale a restart is frequent enough, and it avoids a background goroutine.
	if n, err := db.DeleteExpiredSessions(context.Background(), time.Now().UTC()); err != nil {
		log.Warn("could not prune sessions", "error", err)
	} else if n > 0 {
		log.Info("pruned expired sessions", "count", n)
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-errCh:
		return err
	case sig := <-stop:
		log.Info("shutting down", "signal", sig.String())
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown failed: %w", err)
	}
	log.Info("stopped cleanly")
	return nil
}
