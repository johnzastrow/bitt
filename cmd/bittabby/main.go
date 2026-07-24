// Command bittabby runs the BitTabby server.
//
// DEPLOY-04: templates, stylesheets, and migrations are embedded, so this
// binary runs with no files beside it other than the database it creates.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	// Period boundaries are computed in the instance timezone (SCHED-02), which
	// means time.LoadLocation has to work. A static binary in a scratch
	// container has no system zoneinfo to read, so the database is embedded
	// here rather than left as a deployment prerequisite (DEPLOY-04). It costs
	// roughly 450KB and removes an entire class of "works on my machine".
	_ "time/tzdata"

	"github.com/johnzastrow/bitt/internal/auth"
	"github.com/johnzastrow/bitt/internal/config"
	"github.com/johnzastrow/bitt/internal/ledger"
	"github.com/johnzastrow/bitt/internal/notify"
	"github.com/johnzastrow/bitt/internal/store/sqlite"
	"github.com/johnzastrow/bitt/internal/version"
	"github.com/johnzastrow/bitt/internal/web"
)

func main() {
	if handled, err := runArgs(os.Args[1:]); handled {
		if err != nil {
			fmt.Fprintf(os.Stderr, "bittabby: %v\n", err)
			os.Exit(2)
		}
		return
	}
	if err := run(); err != nil {
		// The detail goes to stderr for the operator; nothing sensitive is
		// included, since config never logs values.
		fmt.Fprintf(os.Stderr, "bittabby: %v\n", err)
		os.Exit(1)
	}
}

// runArgs handles the command line, reporting whether it did something that
// should stop main from starting a server.
//
// The server takes all its configuration from the environment and accepts no
// flags, so historically it ignored argv entirely. That is a worse default than
// it sounds: `bittabby --version` looks like a question and was answered by
// silently starting a server against BITT_DB_PATH, which on one occasion
// migrated a live database that was only meant to be inspected. Anything
// unrecognized is now refused, and the two questions people actually ask are
// answered without touching the database.
func runArgs(args []string) (handled bool, err error) {
	if len(args) == 0 {
		return false, nil
	}
	switch args[0] {
	case "-v", "--version", "version":
		fmt.Println(version.Full())
		return true, nil
	case "-h", "--help", "help":
		fmt.Print(usage)
		return true, nil
	case "--healthcheck", "healthcheck":
		return true, healthcheck()
	}
	return true, fmt.Errorf("unrecognized argument %q\n\n%s", args[0], usage)
}

// usage lists what the binary accepts. Configuration is environment-only, so
// this is mostly a pointer at the variables that matter.
const usage = `bittabby -- self-hosted shared-tab tracker

Usage:
  bittabby                start the server
  bittabby --version      print the version and exit
  bittabby --help         print this message and exit
  bittabby --healthcheck  probe a running server and exit 0 if it is healthy

Configuration is read from the environment:
  BITT_ADDR                 listen address (default :8080)
  BITT_DB_PATH              SQLite database path (default data/bitt.db)
  BITT_TIMEZONE             default instance timezone, used at first-run setup
  BITT_SECURE_COOKIES       set false only for plain HTTP on localhost
  BITT_APPEND_ONLY_TRIGGERS set false only to recover a database by hand

Starting the server applies any pending schema migrations, which are
forward-only. Take a copy first if that matters.
`

// healthcheck probes a running server on this host and reports its state
// through the exit code: 0 healthy, non-zero not.
//
// It exists because the container image has no shell and no curl to probe with.
// Shipping either one to satisfy a HEALTHCHECK would put a whole userland into
// an image that currently holds one static binary, which is a poor trade for a
// GET this binary can perform on itself.
//
// It talks to the loopback address on the configured port rather than to
// BITT_ADDR verbatim, since that is often ":8080" or "0.0.0.0:8080", neither of
// which is a destination.
func healthcheck() error {
	addr := os.Getenv("BITT_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return fmt.Errorf("healthcheck: BITT_ADDR %q has no port", addr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), healthcheckTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://127.0.0.1:"+port+"/healthz", nil)
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	resp, err := (&http.Client{Timeout: healthcheckTimeout}).Do(req)
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// Read and discard so the connection can be reused and nothing is left
	// half-consumed on the server side.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck: server returned %d", resp.StatusCode)
	}
	return nil
}

// healthcheckTimeout bounds the probe. A container healthcheck has its own
// timeout, and hanging until that fires reports "unhealthy" far later than
// necessary.
const healthcheckTimeout = 5 * time.Second

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
		"version", version.Full(),
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
		notify.New(cfg.Notify),
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
