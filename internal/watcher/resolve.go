package watcher

// This file owns the credential-source priority logic shared by
// NewClient. The actual kubeconfig parsing happens in client.go; this
// file decides only WHICH source to use.

import (
	"os"
	"path/filepath"
	"runtime"
)

// credentialSource identifies which credential resolution branch was
// chosen. Order of constants matches the priority documented in the
// spec (D1): flag > in-cluster > env > home.
type credentialSource int

const (
	sourceNone credentialSource = iota
	sourceFlag
	sourceInCluster
	sourceEnv
	sourceHome
)

// String renders the source in the "source: X" log/error tags.
func (s credentialSource) String() string {
	switch s {
	case sourceFlag:
		return "flag"
	case sourceInCluster:
		return "in-cluster"
	case sourceEnv:
		return "env"
	case sourceHome:
		return "kubeconfig"
	default:
		return "none"
	}
}

// sourceProbe abstracts the filesystem and environment checks needed
// for credential resolution. The default production probe wraps
// os.Stat, os.Getenv, and the canonical in-cluster token path. Tests
// supply a deterministic in-memory impl.
type sourceProbe interface {
	fileExists(path string) bool
	kubeconfigEnv() string
	homeKubeconfigPath() string
	inClusterTokenAvailable() bool
}

// resolveSource picks the credential source per the priority in spec
// D1. The second return is the literal flag-supplied path for
// sourceFlag only (empty otherwise). Tests inject probe; production
// callers use defaultProbe{}.
func resolveSource(opts Options, probe sourceProbe) (credentialSource, string, error) {
	if opts.KubeconfigPath != "" {
		if !probe.fileExists(opts.KubeconfigPath) {
			return sourceNone, "", &noCredentialsError{
				flagPath: opts.KubeconfigPath,
			}
		}
		return sourceFlag, opts.KubeconfigPath, nil
	}
	if probe.inClusterTokenAvailable() {
		return sourceInCluster, "", nil
	}
	if probe.kubeconfigEnv() != "" {
		return sourceEnv, "", nil
	}
	if home := probe.homeKubeconfigPath(); home != "" && probe.fileExists(home) {
		return sourceHome, "", nil
	}
	return sourceNone, "", &noCredentialsError{}
}

// defaultProbe is the production sourceProbe.
type defaultProbe struct{}

func (defaultProbe) fileExists(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

func (defaultProbe) kubeconfigEnv() string {
	return os.Getenv("KUBECONFIG")
}

func (defaultProbe) homeKubeconfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".kube", "config")
}

func (defaultProbe) inClusterTokenAvailable() bool {
	if runtime.GOOS != "linux" {
		// rest.InClusterConfig() only succeeds on linux pods; treat
		// presence on other GOOS as a false positive.
		return false
	}
	_, err := os.Stat("/var/run/secrets/kubernetes.io/serviceaccount/token")
	return err == nil
}

// noCredentialsError carries the path that --kubeconfig pointed at
// when it's set but the file is missing. When the struct's zero
// value is used, the error renders the "no credentials found"
// enumeration.
type noCredentialsError struct {
	flagPath string // non-empty ⇒ "--kubeconfig path does not exist: <path>"
}

func (e *noCredentialsError) Error() string {
	if e.flagPath != "" {
		return "--kubeconfig path does not exist: " + e.flagPath
	}
	return "no Kubernetes credentials found: " +
		"tried --kubeconfig (unset), " +
		"in-cluster (no service-account token), " +
		"KUBECONFIG (unset), " +
		"~/.kube/config (does not exist); " +
		"pass --no-cluster to skip cluster watching"
}
