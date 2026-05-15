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

	"github.com/northwatchlabs/northwatch/internal/config"
	"github.com/northwatchlabs/northwatch/internal/server"
	"github.com/northwatchlabs/northwatch/internal/store"
	"github.com/northwatchlabs/northwatch/internal/watcher"
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
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "serve flags:")
	fmt.Fprintln(os.Stderr, "  --addr             HTTP listen address (default :8080)")
	fmt.Fprintln(os.Stderr, "  --db               SQLite database file path (default ./northwatch.db)")
	fmt.Fprintln(os.Stderr, "  --config           Path to YAML component config (default northwatch.yaml)")
	fmt.Fprintln(os.Stderr, "  --allow-deactivate Allow boot to deactivate components no longer in --config")
	fmt.Fprintln(os.Stderr, "  --kubeconfig       Explicit kubeconfig path (overrides in-cluster credentials)")
	fmt.Fprintln(os.Stderr, "  --kube-context     Context within the resolved kubeconfig")
	fmt.Fprintln(os.Stderr, "  --no-cluster       Skip cluster connectivity (local-only run)")
}

func serveCmd(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", envOr("NORTHWATCH_ADDR", defaultAddr), "HTTP listen address")
	dbPath := fs.String("db", envOr("NORTHWATCH_DB", defaultDB), "SQLite database file path")
	configPath := fs.String("config",
		envOr("NORTHWATCH_CONFIG", "northwatch.yaml"),
		"Path to YAML config declaring watched components")
	allowDeactivate := fs.Bool("allow-deactivate",
		envOrBool("NORTHWATCH_ALLOW_DEACTIVATE", false),
		"Allow boot to deactivate components no longer in --config")
	kubeconfigPath := fs.String("kubeconfig",
		envOr("NORTHWATCH_KUBECONFIG", ""),
		"Explicit kubeconfig path (overrides in-cluster credentials)")
	kubeContext := fs.String("kube-context",
		envOr("NORTHWATCH_KUBE_CONTEXT", ""),
		"Context within the resolved kubeconfig")
	noCluster := fs.Bool("no-cluster",
		envOrBool("NORTHWATCH_NO_CLUSTER", false),
		"Skip cluster connectivity (local-only run)")
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

	if code := runConfigSync(ctx, st, *configPath, *allowDeactivate, logger); code != 0 {
		return code
	}

	kc, err := runClusterInit(ctx, *noCluster, *kubeconfigPath, *kubeContext, logger)
	if err != nil {
		logger.Error("kubernetes client init failed", "err", err)
		return 1
	}
	_ = kc // Consumed by watchers (#6+); placeholder until then.

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

// envOrBool reads a bool-ish env var. "1", "true", "yes"
// (case-insensitive) → true; "0", "false", "no" and any other
// recognized-but-explicit value → false. Unset or empty → def
// (empty is treated as unset to match standard shell convention,
// so `VAR=` doesn't silently flip a default).
func envOrBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

// runConfigSync loads configPath, translates Specs into ComponentSpecs,
// and calls SyncComponents. Returns 0 on success or a non-zero exit
// code on any failure (the matching error is already logged). Pulled
// out of serveCmd so it's testable without starting the HTTP server.
func runConfigSync(
	ctx context.Context,
	st store.Store,
	configPath string,
	allowDeactivate bool,
	logger *slog.Logger,
) int {
	cfg, err := config.Load(configPath)
	if err != nil {
		logger.Error("config load failed", "err", err, "path", configPath)
		return 1
	}

	specs := make([]store.ComponentSpec, 0, len(cfg.Components))
	for _, c := range cfg.Components {
		dn := c.DisplayName
		if dn == "" {
			dn = c.Name
		}
		specs = append(specs, store.ComponentSpec{
			Kind:        c.Kind,
			Namespace:   c.Namespace,
			Name:        c.Name,
			DisplayName: dn,
		})
	}

	deactivated, err := st.SyncComponents(ctx, specs, allowDeactivate)
	var refused *store.DeactivationRefusedError
	if errors.As(err, &refused) {
		logger.Error("refusing to deactivate components without --allow-deactivate",
			"would_deactivate", len(refused.IDs),
			"ids", refused.IDs,
			"config", configPath,
			"hint", "verify the config is correct; re-run with --allow-deactivate to confirm",
		)
		return 1
	}
	if err != nil {
		logger.Error("sync components failed", "err", err)
		return 1
	}
	logger.Info("components synced",
		"active", len(specs),
		"deactivated", deactivated,
		"config", configPath,
	)
	return 0
}

// runClusterInit constructs the shared watcher.Client unless
// --no-cluster is set. Pulled out of serveCmd so tests can exercise
// the --no-cluster short-circuit without starting the HTTP server.
// Returns (nil, nil) when --no-cluster is true. Errors are logged by
// the caller.
func runClusterInit(
	ctx context.Context,
	noCluster bool,
	kubeconfigPath string,
	kubeContext string,
	logger *slog.Logger,
) (*watcher.Client, error) {
	if noCluster {
		logger.Info("cluster watching disabled (--no-cluster)")
		return nil, nil
	}
	return watcher.NewClient(ctx, watcher.Options{
		KubeconfigPath: kubeconfigPath,
		KubeContext:    kubeContext,
		Logger:         logger,
	})
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
