// Package watcher derives component status from Kubernetes resources.
// It owns the shared *Client used by per-kind watchers (#6–#8) and the
// credential-source priority logic that resolves them.
package watcher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// pingTimeout caps how long the boot ServerVersion() call waits before
// we declare the cluster unreachable. The timeout is applied at the
// transport layer because discovery.ServerVersion() has no
// context-accepting variant in client-go v0.35.
const pingTimeout = 10 * time.Second

// Options configures NewClient. KubeconfigPath and KubeContext come
// from --kubeconfig and --kube-context (or their NORTHWATCH_* env
// defaults). Logger is required.
type Options struct {
	KubeconfigPath string
	KubeContext    string
	Logger         *slog.Logger
}

// Client is the shared Kubernetes connection NorthWatch holds for the
// lifetime of `serve`. Future watchers (#6–#8) consume Config and
// Clientset to build their own typed/dynamic clients.
type Client struct {
	Config     *rest.Config
	Clientset  kubernetes.Interface
	Context    string        // kube-context name, or "in-cluster"
	ServerInfo *version.Info // result of the boot ping
}

// NewClient resolves credentials, builds *rest.Config, runs a single
// connectivity ping against the apiserver, and returns a populated
// Client. The ping uses a 10s transport timeout; a slow apiserver
// surfaces as a deadline-exceeded error rather than hanging boot.
func NewClient(ctx context.Context, opts Options) (*Client, error) {
	if opts.Logger == nil {
		return nil, errors.New("watcher.NewClient: Options.Logger is required")
	}

	cfg, source, ctxName, err := resolveRestConfig(opts, defaultProbe{})
	if err != nil {
		return nil, err
	}

	// Apply transport timeout to the cloned config used for the ping.
	// The clientset built later intentionally uses the un-timeout'd cfg
	// — watchers (#6+) need long-lived list/watch streams. The ping
	// timeout applies ONLY to pingCfg.
	pingCfg := rest.CopyConfig(cfg)
	pingCfg.Timeout = pingTimeout

	disc, err := discovery.NewDiscoveryClientForConfig(pingCfg)
	if err != nil {
		return nil, fmt.Errorf("build discovery client: %w", err)
	}

	// discovery.ServerVersion() has no context-accepting variant in
	// client-go v0.35, so race it against ctx.Done() in a goroutine to
	// honour parent cancellation (SIGINT/SIGTERM). The transport
	// timeout on pingCfg still caps the goroutine's lifetime.
	type pingResult struct {
		info *version.Info
		err  error
	}
	resultCh := make(chan pingResult, 1)
	go func() {
		info, err := disc.ServerVersion()
		resultCh <- pingResult{info: info, err: err}
	}()
	var info *version.Info
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf(
			"cluster connectivity check cancelled (source: %s, context: %s): %w",
			source, ctxName, ctx.Err(),
		)
	case r := <-resultCh:
		if r.err != nil {
			return nil, fmt.Errorf(
				"cluster connectivity check failed (source: %s, context: %s): %w",
				source, ctxName, r.err,
			)
		}
		info = r.info
	}

	// Clientset built after the ping so a failing ping doesn't leave a
	// half-constructed client around for the caller to misuse.
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build clientset: %w", err)
	}

	opts.Logger.Info("connected to cluster",
		"version", info.GitVersion,
		"context", ctxName,
		"source", source.String(),
	)

	return &Client{
		Config:     cfg,
		Clientset:  cs,
		Context:    ctxName,
		ServerInfo: info,
	}, nil
}

// resolveRestConfig is the testable seam: it does everything up to but
// not including the discovery ping. Returns the built *rest.Config,
// the source enum, and the resolved kube-context name ("in-cluster"
// for the in-cluster branch).
func resolveRestConfig(opts Options, probe sourceProbe) (*rest.Config, credentialSource, string, error) {
	src, flagPath, err := resolveSource(opts, probe)
	if err != nil {
		return nil, src, "", err
	}

	switch src {
	case sourceInCluster:
		cfg, err := rest.InClusterConfig()
		if err != nil {
			return nil, src, "", fmt.Errorf("build in-cluster config: %w", err)
		}
		if opts.KubeContext != "" {
			opts.Logger.Warn("--kube-context ignored: resolved to in-cluster credentials")
		}
		return cfg, src, "in-cluster", nil

	case sourceFlag, sourceEnv, sourceHome:
		rules := loadingRulesFor(src, flagPath, probe)
		overrides := &clientcmd.ConfigOverrides{}
		if opts.KubeContext != "" {
			overrides.CurrentContext = opts.KubeContext
		}
		cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)

		cfg, err := cc.ClientConfig()
		if err != nil {
			return nil, src, "", fmt.Errorf("load kubeconfig: %w", err)
		}

		raw, err := cc.RawConfig()
		if err != nil {
			return nil, src, "", fmt.Errorf("read kubeconfig: %w", err)
		}
		ctxName := opts.KubeContext
		if ctxName == "" {
			ctxName = raw.CurrentContext
		}
		return cfg, src, ctxName, nil
	}

	return nil, sourceNone, "", errors.New("watcher: unreachable credential source")
}

// loadingRulesFor returns the client-go loading rules appropriate to
// the resolved source. sourceFlag uses ExplicitPath (single file,
// missing → error). sourceEnv scopes the precedence to KUBECONFIG's
// own paths (split by os.PathListSeparator) so multi-path values are
// merged correctly without silently falling through to ~/.kube/config
// — that fallback would make source=env misreport reality. sourceHome
// scopes the rules to just the home kubeconfig file.
func loadingRulesFor(src credentialSource, flagPath string, probe sourceProbe) *clientcmd.ClientConfigLoadingRules {
	switch src {
	case sourceFlag:
		return &clientcmd.ClientConfigLoadingRules{ExplicitPath: flagPath}
	case sourceEnv:
		return &clientcmd.ClientConfigLoadingRules{
			Precedence: filepath.SplitList(probe.kubeconfigEnv()),
		}
	case sourceHome:
		return &clientcmd.ClientConfigLoadingRules{
			Precedence: []string{probe.homeKubeconfigPath()},
		}
	}
	panic(fmt.Sprintf("watcher: loadingRulesFor called with unsupported source %v", src))
}
