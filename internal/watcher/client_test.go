package watcher

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testLogger returns a slog logger that discards output, for tests
// that don't care about log assertions.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// writeKubeconfig writes body to a tempfile and returns the path.
// Body is raw YAML — caller supplies the full kubeconfig content.
func writeKubeconfig(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// minimalKubeconfig is a valid kubeconfig with one context. The
// server URL points at a non-routable IP — kubeconfig parsing
// succeeds, but anything that calls the apiserver would fail. That
// matches resolveRestConfig's contract (no apiserver call).
const minimalKubeconfig = `apiVersion: v1
kind: Config
current-context: dev-ctx
contexts:
- name: dev-ctx
  context:
    cluster: dev
    user: dev-user
- name: other-ctx
  context:
    cluster: dev
    user: dev-user
clusters:
- name: dev
  cluster:
    server: https://127.0.0.1:1
users:
- name: dev-user
  user:
    token: dummy-token
`

// fileProbe is a sourceProbe variant that delegates fileExists to the
// real filesystem so kubeconfig-on-disk tests work, while keeping the
// other fields injectable.
type fileProbe struct {
	envValue  string
	homePath  string
	inCluster bool
}

func (p fileProbe) fileExists(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}
func (p fileProbe) kubeconfigEnv() string         { return p.envValue }
func (p fileProbe) homeKubeconfigPath() string    { return p.homePath }
func (p fileProbe) inClusterTokenAvailable() bool { return p.inCluster }

func TestResolveRestConfig_FlagPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	kc := writeKubeconfig(t, dir, "kubeconfig", minimalKubeconfig)

	opts := Options{
		KubeconfigPath: kc,
		Logger:         testLogger(),
	}
	cfg, src, ctxName, err := resolveRestConfig(opts, fileProbe{})
	if err != nil {
		t.Fatalf("resolveRestConfig: %v", err)
	}
	if cfg == nil {
		t.Fatal("cfg is nil")
	}
	if src != sourceFlag {
		t.Errorf("src = %v, want sourceFlag", src)
	}
	if ctxName != "dev-ctx" {
		t.Errorf("ctxName = %q, want %q", ctxName, "dev-ctx")
	}
}

func TestResolveRestConfig_KubeContextOverride(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	kc := writeKubeconfig(t, dir, "kubeconfig", minimalKubeconfig)

	opts := Options{
		KubeconfigPath: kc,
		KubeContext:    "other-ctx",
		Logger:         testLogger(),
	}
	_, _, ctxName, err := resolveRestConfig(opts, fileProbe{})
	if err != nil {
		t.Fatalf("resolveRestConfig: %v", err)
	}
	if ctxName != "other-ctx" {
		t.Errorf("ctxName = %q, want %q", ctxName, "other-ctx")
	}
}

func TestResolveRestConfig_KubeContextMissing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	kc := writeKubeconfig(t, dir, "kubeconfig", minimalKubeconfig)

	opts := Options{
		KubeconfigPath: kc,
		KubeContext:    "nope-ctx",
		Logger:         testLogger(),
	}
	_, _, _, err := resolveRestConfig(opts, fileProbe{})
	if err == nil {
		t.Fatal("got nil err, want error from clientcmd for missing context")
	}
}

func TestResolveRestConfig_KubeconfigEnvMultiPath(t *testing.T) {
	// Cannot use t.Parallel() with t.Setenv below — Go test framework
	// rejects parallel + env mutation as a race hazard.
	dir := t.TempDir()

	// Split a valid config across two files: file A has cluster +
	// context skeleton, file B has the user. client-go merges them.
	fileA := `apiVersion: v1
kind: Config
current-context: merged-ctx
contexts:
- name: merged-ctx
  context:
    cluster: dev
    user: dev-user
clusters:
- name: dev
  cluster:
    server: https://127.0.0.1:1
`
	fileB := `apiVersion: v1
kind: Config
users:
- name: dev-user
  user:
    token: dummy-token
`
	pa := writeKubeconfig(t, dir, "a.yaml", fileA)
	pb := writeKubeconfig(t, dir, "b.yaml", fileB)

	// KUBECONFIG with colon-separated paths is the standard
	// multi-file convention. resolveRestConfig must hand parsing to
	// client-go so the merge happens correctly.
	t.Setenv("KUBECONFIG", pa+string(os.PathListSeparator)+pb)

	opts := Options{Logger: testLogger()}
	cfg, src, ctxName, err := resolveRestConfig(opts, fileProbe{envValue: pa + string(os.PathListSeparator) + pb})
	if err != nil {
		t.Fatalf("resolveRestConfig: %v", err)
	}
	if cfg == nil {
		t.Fatal("cfg is nil")
	}
	if src != sourceEnv {
		t.Errorf("src = %v, want sourceEnv", src)
	}
	if ctxName != "merged-ctx" {
		t.Errorf("ctxName = %q, want %q", ctxName, "merged-ctx")
	}
}

func TestResolveRestConfig_NoCredentialsError(t *testing.T) {
	t.Parallel()
	opts := Options{Logger: testLogger()}
	_, _, _, err := resolveRestConfig(opts, fileProbe{}) // nothing available
	if err == nil {
		t.Fatal("got nil err, want noCredentialsError")
	}
	want := "no Kubernetes credentials found"
	if got := err.Error(); !strings.Contains(got, want) {
		t.Errorf("err = %q, want substring %q", got, want)
	}
}

func TestResolveRestConfig_FlagMissingFile(t *testing.T) {
	t.Parallel()
	opts := Options{
		KubeconfigPath: "/tmp/does-not-exist-northwatch-test",
		Logger:         testLogger(),
	}
	_, _, _, err := resolveRestConfig(opts, fileProbe{})
	if err == nil {
		t.Fatal("got nil err, want error for missing --kubeconfig path")
	}
	want := "--kubeconfig path does not exist"
	if got := err.Error(); !strings.Contains(got, want) {
		t.Errorf("err = %q, want substring %q", got, want)
	}
}
