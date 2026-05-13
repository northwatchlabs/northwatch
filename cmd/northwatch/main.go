// Command northwatch is the status-page server binary.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/northwatchlabs/northwatch/internal/server"
	"github.com/northwatchlabs/northwatch/internal/store"
)

const (
	defaultAddr = ":8080"
	defaultDB   = "./northwatch.db"
)

func main() {
	if len(os.Args) < 2 {
		os.Exit(serveCmd(nil))
	}
	switch os.Args[1] {
	case "serve":
		os.Exit(serveCmd(os.Args[2:]))
	case "migrate":
		os.Exit(migrateCmd(os.Args[2:]))
	case "-h", "--help", "help":
		usage()
		os.Exit(0)
	default:
		if strings.HasPrefix(os.Args[1], "-") {
			os.Exit(serveCmd(os.Args[1:]))
		}
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "Usage: northwatch [subcommand] [flags]")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "If no subcommand is given, `serve` is used.")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Subcommands:")
	fmt.Fprintln(os.Stderr, "  serve    Run the status-page HTTP server (default)")
	fmt.Fprintln(os.Stderr, "  migrate  Apply pending database migrations and exit")
}

func serveCmd(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", envOr("NORTHWATCH_ADDR", defaultAddr), "HTTP listen address")
	dbPath := fs.String("db", envOr("NORTHWATCH_DB", defaultDB), "SQLite database file path")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.OpenSQLite(ctx, *dbPath)
	if err != nil {
		logger.Error("open store failed", "err", err, "db", *dbPath)
		return 1
	}
	defer func() { _ = st.Close() }()

	if err := st.Migrate(ctx); err != nil {
		logger.Error("migrate failed", "err", err)
		return 1
	}

	h, err := server.New(logger, st)
	if err != nil {
		logger.Error("server init failed", "err", err)
		return 1
	}

	srv := &http.Server{
		Addr:              normalizeAddr(*addr),
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", srv.Addr, "db", *dbPath)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server exited", "err", err)
			return 1
		}
	case <-ctx.Done():
		logger.Info("shutdown signal received")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("graceful shutdown failed", "err", err)
			return 1
		}
	}
	return 0
}

func migrateCmd(args []string) int {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	dbPath := fs.String("db", envOr("NORTHWATCH_DB", defaultDB), "SQLite database file path")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.OpenSQLite(ctx, *dbPath)
	if err != nil {
		logger.Error("open store failed", "err", err, "db", *dbPath)
		return 1
	}
	defer func() { _ = st.Close() }()

	if err := st.Migrate(ctx); err != nil {
		logger.Error("migrate failed", "err", err)
		return 1
	}
	logger.Info("migrations applied", "db", *dbPath)
	return 0
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func normalizeAddr(addr string) string {
	if addr == "" {
		return defaultAddr
	}
	if !strings.Contains(addr, ":") && isAllDigits(addr) {
		return ":" + addr
	}
	return addr
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
