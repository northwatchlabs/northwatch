//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

var jsonUnmarshal = json.Unmarshal

// env holds resolved e2e runtime configuration.
type env struct {
	cluster string // kind cluster name (E2E_CLUSTER, default northwatch-e2e)
	context string // kubectl context name (kind-<cluster>)
	port    string // host port for kubectl port-forward (E2E_PORT, default 18081)
	apiBase string // base URL for HTTP calls (http://127.0.0.1:<port>)
	token   string // bearer token; must be >=16 chars to match server validation
}

const (
	releaseName       = "nw"
	releaseNamespace  = "northwatch"
	workloadNamespace = "northwatch-e2e-workload"
	componentID       = "Deployment/northwatch-e2e-workload/api-gateway"
	bearerToken       = "e2e-test-token-00" // 17 chars; server enforces >=16
	incidentTitle     = "e2e: scaled to zero"
)

func envFromOS() env {
	cluster := os.Getenv("E2E_CLUSTER")
	if cluster == "" {
		cluster = "northwatch-e2e"
	}
	port := os.Getenv("E2E_PORT")
	if port == "" {
		port = "18081"
	}
	return env{
		cluster: cluster,
		context: "kind-" + cluster,
		port:    port,
		apiBase: "http://127.0.0.1:" + port,
		token:   bearerToken,
	}
}

// requireContext fails the test if the configured kube context does
// not exist. Without this, bare kubectl/helm calls would target
// whatever the developer's current context happens to be.
func requireContext(t *testing.T, e env) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "kubectl", "config", "get-contexts", "-o", "name").CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl config get-contexts failed: %v\n%s", err, out)
	}
	for _, name := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if name == e.context {
			return
		}
	}
	t.Fatalf("kube context %q not found; available contexts:\n%s", e.context, out)
}

// runCmd runs an external command with a per-call timeout. Returns
// combined stdout+stderr on both success and failure so callers can
// log it in diagnostics.
func runCmd(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.CombinedOutput()
}

// kubectl runs `kubectl --context <ctx> <args>` with a 30s default
// timeout. Use longer-timeout variants for `wait` / `scale` etc.
func kubectl(t *testing.T, e env, args ...string) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	full := append([]string{"--context", e.context}, args...)
	return runCmd(ctx, "kubectl", full...)
}

// kubectlT lets the caller set a custom timeout (used for `wait`).
func kubectlT(t *testing.T, e env, timeout time.Duration, args ...string) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	full := append([]string{"--context", e.context}, args...)
	return runCmd(ctx, "kubectl", full...)
}

// helm runs `helm <subcommand> --kube-context <ctx> <args>` with a
// caller-supplied timeout. Helm gets its own helper because the
// outer context must exceed Helm's internal --timeout by ~30s so
// Helm's error surfaces cleanly instead of being preempted.
func helm(t *testing.T, e env, timeout time.Duration, args ...string) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	full := append([]string{args[0], "--kube-context", e.context}, args[1:]...)
	return runCmd(ctx, "helm", full...)
}

// httpGet performs GET with a 10s timeout. Body is returned as a
// string so substring assertions are easy.
func httpGet(t *testing.T, url string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("http.NewRequest %s: %v", url, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("GET %s: read body: %v", url, err)
	}
	return resp.StatusCode, string(body)
}

// httpPostJSON performs POST application/json with bearer auth and
// a 10s timeout.
func httpPostJSON(t *testing.T, url, token, body string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("http.NewRequest %s: %v", url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("POST %s: read body: %v", url, err)
	}
	return resp.StatusCode, string(out)
}

// run is a thin convenience: fail the test with command output on
// non-zero exit.
func run(t *testing.T, label string, out []byte, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s failed: %v\n%s", label, err, bytes.TrimSpace(out))
	}
}

// pollUntil calls fn every interval until it returns nil or the
// deadline elapses. On timeout it dumps diagnostics synchronously
// via diag.fatal so cluster state is captured before any cleanup
// runs. Without the diag route, the safety-net t.Cleanup would fire
// last in LIFO order — after resource teardowns — and the dump
// would describe a torn-down cluster.
func pollUntil(t *testing.T, diag *diagBundle, label string, deadline, interval time.Duration, fn func() error) {
	t.Helper()
	end := time.Now().Add(deadline)
	var lastErr error
	for {
		lastErr = fn()
		if lastErr == nil {
			return
		}
		if time.Now().After(end) {
			diag.fatal(t, "%s: timed out after %s; last error: %v", label, deadline, lastErr)
		}
		time.Sleep(interval)
	}
}

// diagBundle captures the bits we want to dump on failure. The
// HTTP bodies are the most recent values observed by the polling
// helpers, so callers can update them as the test progresses.
type diagBundle struct {
	env            env
	lastComponents string
	lastIncidents  string
}

// newDiag returns a bundle and registers the diagnostic dump as a
// t.Cleanup. The dump checks t.Failed() and is a no-op on success.
// Registering this BEFORE any other t.Cleanup means it runs LAST
// in LIFO order — i.e., after resource cleanups remove the cluster
// state. We accept that for the safety-net role; the primary path
// is callers explicitly invoking diag.Dump(t) before t.Fatal via
// the fatal helper below.
func newDiag(t *testing.T, e env) *diagBundle {
	t.Helper()
	d := &diagBundle{env: e}
	t.Cleanup(func() {
		if t.Failed() {
			d.Dump(t)
		}
	})
	return d
}

// Dump emits the diagnostic bundle via t.Logf. Safe to call
// multiple times; individual command failures are tolerated.
func (d *diagBundle) Dump(t *testing.T) {
	t.Helper()
	e := d.env

	dump := func(label string, out []byte, err error) {
		if err != nil {
			t.Logf("[diag] %s: error: %v\n%s", label, err, out)
			return
		}
		t.Logf("[diag] %s:\n%s", label, out)
	}

	out, err := helm(t, e, 15*time.Second, "status", releaseName, "--namespace", releaseNamespace)
	dump("helm status", out, err)

	out, err = kubectl(t, e, "-n", releaseNamespace, "get", "pods,deploy,svc,events", "--sort-by=.lastTimestamp")
	dump("kubectl get -n "+releaseNamespace, out, err)

	out, err = kubectl(t, e, "-n", releaseNamespace, "describe", "pods,deploy")
	dump("kubectl describe -n "+releaseNamespace, out, err)

	out, err = kubectl(t, e, "-n", releaseNamespace, "logs", "deploy/"+releaseName+"-northwatch", "--tail=200")
	dump("kubectl logs", out, err)

	out, err = kubectl(t, e, "-n", releaseNamespace, "logs", "deploy/"+releaseName+"-northwatch", "--previous", "--tail=200")
	dump("kubectl logs --previous", out, err)

	out, err = kubectl(t, e, "-n", workloadNamespace, "describe", "deploy/api-gateway")
	dump("kubectl describe -n "+workloadNamespace+" deploy/api-gateway", out, err)

	if d.lastComponents != "" {
		t.Logf("[diag] last /api/components: %s", d.lastComponents)
	}
	if d.lastIncidents != "" {
		t.Logf("[diag] last /api/incidents: %s", d.lastIncidents)
	}
}

// fatal dumps the bundle synchronously and then calls t.Fatalf.
// Use this instead of t.Fatal for any failure that happens while
// cluster state is still useful to inspect.
func (d *diagBundle) fatal(t *testing.T, format string, args ...any) {
	t.Helper()
	d.Dump(t)
	t.Fatalf(format, args...)
}

// preInstallReset wipes any stale NorthWatch release / namespace /
// cluster-scoped RBAC. Safe to call against a clean cluster (each
// step is independently no-op-on-missing).
func preInstallReset(t *testing.T, e env) {
	t.Helper()

	// helm uninstall (only if installed)
	if _, statusErr := helm(t, e, 15*time.Second, "status", releaseName, "--namespace", releaseNamespace); statusErr == nil {
		out, err := helm(t, e, 90*time.Second,
			"uninstall", releaseName,
			"--namespace", releaseNamespace,
			"--wait", "--timeout", "60s")
		if err != nil {
			t.Logf("preInstallReset: helm uninstall: %v\n%s", err, out)
		}
	}

	// delete cluster-scoped RBAC the chart owns
	if out, err := kubectl(t, e, "delete", "clusterrole,clusterrolebinding",
		"-l", "app.kubernetes.io/instance="+releaseName, "--ignore-not-found"); err != nil {
		t.Logf("preInstallReset: delete RBAC: %v\n%s", err, out)
	}

	// delete the namespace, waiting for it to finish so the next
	// install --create-namespace doesn't race with a Terminating
	// state.
	if out, err := kubectlT(t, e, 90*time.Second, "delete", "namespace", releaseNamespace,
		"--ignore-not-found", "--wait"); err != nil {
		t.Logf("preInstallReset: delete namespace: %v\n%s", err, out)
	}
}

// teardownNorthwatch is the same logic registered as t.Cleanup
// after install. Uses --wait=false on the namespace delete because
// the next test run's preInstallReset will wait for it.
func teardownNorthwatch(t *testing.T, e env) {
	t.Helper()
	if out, err := helm(t, e, 90*time.Second,
		"uninstall", releaseName,
		"--namespace", releaseNamespace,
		"--wait", "--timeout", "60s"); err != nil {
		t.Logf("teardown: helm uninstall: %v\n%s", err, out)
	}
	if out, err := kubectl(t, e, "delete", "clusterrole,clusterrolebinding",
		"-l", "app.kubernetes.io/instance="+releaseName, "--ignore-not-found"); err != nil {
		t.Logf("teardown: delete RBAC: %v\n%s", err, out)
	}
	if out, err := kubectl(t, e, "delete", "namespace", releaseNamespace,
		"--ignore-not-found", "--wait=false"); err != nil {
		t.Logf("teardown: delete namespace: %v\n%s", err, out)
	}
}

// portForward starts kubectl port-forward in the background and
// returns a stop function (registered as t.Cleanup by the caller).
// It blocks until either the port responds to GET /healthz or the
// timeout elapses.
func portForward(t *testing.T, e env) func() {
	t.Helper()

	args := []string{
		"--context", e.context,
		"-n", releaseNamespace,
		"port-forward",
		"svc/" + releaseName + "-northwatch",
		e.port + ":8080",
	}
	cmd := exec.Command("kubectl", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("port-forward stdout pipe: %v", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("port-forward stderr pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("port-forward start: %v", err)
	}

	// Drain stdout/stderr so the buffers don't block kubectl.
	go func() { _, _ = io.Copy(io.Discard, stdout) }()
	go func() { _, _ = io.Copy(io.Discard, stderr) }()

	stop := func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}

	// Wait until /healthz responds (max 15s).
	deadline := time.Now().Add(15 * time.Second)
	for {
		if code, _ := tryGet(t, e.apiBase+"/healthz"); code == http.StatusOK {
			return stop
		}
		if time.Now().After(deadline) {
			stop()
			t.Fatalf("port-forward: /healthz never returned 200 within 15s")
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// tryGet is like httpGet but returns (0, "") on transport errors
// instead of failing the test. Used by readiness loops.
func tryGet(t *testing.T, url string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, ""
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// apiComponent matches the wire shape returned by GET
// /api/components. Only the fields the e2e cares about.
type apiComponent struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// fetchComponents records the raw body on diag and returns parsed
// components. Returns an error (rather than calling t.Fatal) so it
// can be used inside polling loops — uses tryGet so transient port-
// forward blips are returned as retryable errors instead of aborting
// the whole test.
func fetchComponents(t *testing.T, e env, diag *diagBundle) ([]apiComponent, error) {
	t.Helper()
	code, body := tryGet(t, e.apiBase+"/api/components")
	diag.lastComponents = body
	if code == 0 {
		return nil, fmt.Errorf("GET /api/components: transport error")
	}
	if code != 200 {
		return nil, fmt.Errorf("GET /api/components: status %d body=%s", code, body)
	}
	var comps []apiComponent
	if err := jsonUnmarshal([]byte(body), &comps); err != nil {
		return nil, fmt.Errorf("decode /api/components: %v body=%s", err, body)
	}
	return comps, nil
}

// findAPIGateway returns the api-gateway component or an error if
// missing.
func findAPIGateway(comps []apiComponent) (apiComponent, error) {
	for _, c := range comps {
		if c.Name == "api-gateway" {
			return c, nil
		}
	}
	return apiComponent{}, fmt.Errorf("api-gateway component not present in /api/components")
}
