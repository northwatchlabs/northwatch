package main

import (
	"context"
	"flag"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/northwatchlabs/northwatch/internal/store"
)

func TestNormalizeAddr(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"", defaultAddr},
		{":8080", ":8080"},
		{"8080", ":8080"},
		{"127.0.0.1:8080", "127.0.0.1:8080"},
		// non-numeric, no colon → pass through so net.Listen surfaces
		// its own error rather than us producing ":localhost".
		{"localhost", "localhost"},
		{"abc123", "abc123"},
	}
	for _, tc := range cases {
		if got := normalizeAddr(tc.in); got != tc.want {
			t.Errorf("normalizeAddr(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// testStoreWithMigrate opens an in-memory store with migrations applied.
func testStoreWithMigrate(t *testing.T) *store.SQLite {
	t.Helper()
	ctx := context.Background()
	st, err := store.OpenSQLite(ctx, ":memory:")
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return st
}

// writeConfig writes body to a tempfile in t.TempDir() and returns
// the path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "northwatch.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	os.Stdout = w

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("Close pipe writer: %v", err)
	}
	os.Stdout = old
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll stdout: %v", err)
	}
	return string(out)
}

const cfgAB = `
components:
  - kind: Deployment
    namespace: default
    name: a
    displayName: A
  - kind: Deployment
    namespace: default
    name: b
    displayName: B
`
const cfgAOnly = `
components:
  - kind: Deployment
    namespace: default
    name: a
    displayName: A
`

func TestRunConfigSync_NoDeactivationsNoFlagNeeded(t *testing.T) {
	st := testStoreWithMigrate(t)
	ctx := context.Background()
	cfgPath := writeConfig(t, cfgAB)

	// Initial run seeds both. No deactivations.
	_, code := runConfigSync(ctx, st, cfgPath, false, testLogger())
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	// Second run with same config — still no deactivations, no flag needed.
	_, code = runConfigSync(ctx, st, cfgPath, false, testLogger())
	if code != 0 {
		t.Errorf("second code = %d, want 0", code)
	}
}

func TestRunConfigSync_RefusesDeactivationWithoutFlag(t *testing.T) {
	st := testStoreWithMigrate(t)
	ctx := context.Background()

	// Seed both via cfgAB.
	if _, code := runConfigSync(ctx, st, writeConfig(t, cfgAB), false, testLogger()); code != 0 {
		t.Fatalf("seed: code = %d", code)
	}

	// Now sync with cfgAOnly (b absent) and no flag — must refuse.
	_, code := runConfigSync(ctx, st, writeConfig(t, cfgAOnly), false, testLogger())
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}

	// DB unchanged: both still active.
	got, err := st.ListComponents(ctx)
	if err != nil {
		t.Fatalf("ListComponents: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("len(got) = %d, want 2 (both still active)", len(got))
	}
}

func TestRunConfigSync_AllowsDeactivationWithFlag(t *testing.T) {
	st := testStoreWithMigrate(t)
	ctx := context.Background()

	if _, code := runConfigSync(ctx, st, writeConfig(t, cfgAB), false, testLogger()); code != 0 {
		t.Fatalf("seed: code = %d", code)
	}

	_, code := runConfigSync(ctx, st, writeConfig(t, cfgAOnly), true, testLogger())
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}

	got, err := st.ListComponents(ctx)
	if err != nil {
		t.Fatalf("ListComponents: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("len(got) = %d, want 1 (b inactive, filtered out)", len(got))
	}
}

func TestRunConfigSync_MissingConfig(t *testing.T) {
	st := testStoreWithMigrate(t)
	_, code := runConfigSync(context.Background(), st, "/does/not/exist.yaml", false, testLogger())
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
}

func TestResolveAPIToken(t *testing.T) {
	cases := []struct {
		name    string
		envSet  bool
		envVal  string
		args    []string
		want    string
		wantErr bool
	}{
		{name: "unset env and absent flag disables writes", want: ""},
		{name: "empty env is invalid", envSet: true, envVal: "", wantErr: true},
		{name: "non-empty env is used", envSet: true, envVal: "env-token-123456", want: "env-token-123456"},
		{name: "empty flag is invalid", envSet: true, envVal: "env-token-123456", args: []string{"--api-token", ""}, wantErr: true},
		{name: "flag wins over env", envSet: true, envVal: "env-token-123456", args: []string{"--api-token", "flag-token-123456"}, want: "flag-token-123456"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const key = "NORTHWATCH_API_TOKEN"
			if tc.envSet {
				t.Setenv(key, tc.envVal)
			} else {
				t.Setenv(key, "ignored")
				if err := os.Unsetenv(key); err != nil {
					t.Fatalf("Unsetenv: %v", err)
				}
			}

			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fs.String("api-token", "", "")
			if err := fs.Parse(tc.args); err != nil {
				t.Fatalf("Parse: %v", err)
			}

			got, err := resolveAPIToken(fs)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolveAPIToken() err = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveAPIToken() err = %v, want nil", err)
			}
			if got != tc.want {
				t.Errorf("resolveAPIToken() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEnvOrBool(t *testing.T) {
	cases := []struct {
		name string
		val  string // empty string here means "unset"
		set  bool   // true to call t.Setenv, false to call Unsetenv (with cleanup)
		def  bool
		want bool
	}{
		{name: "true", val: "true", set: true, def: false, want: true},
		{name: "yes uppercase", val: "YES", set: true, def: false, want: true},
		{name: "one", val: "1", set: true, def: false, want: true},
		{name: "no overrides default true", val: "no", set: true, def: true, want: false},
		{name: "garbage", val: "truee", set: true, def: false, want: false},
		{name: "empty string falls back like unset", val: "", set: true, def: true, want: true},
		{name: "empty string with def false falls back", val: "", set: true, def: false, want: false},
		{name: "unset falls back", val: "", set: false, def: true, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const key = "NW_TEST_ENVORBOOL"
			if tc.set {
				t.Setenv(key, tc.val)
			} else {
				// Ensure unset. Use t.Setenv with a value first so cleanup
				// restores prior state, then unset for the assertion.
				t.Setenv(key, "ignored")
				if err := os.Unsetenv(key); err != nil {
					t.Fatalf("Unsetenv: %v", err)
				}
			}
			if got := envOrBool(key, tc.def); got != tc.want {
				t.Errorf("envOrBool(%q, %v) = %v, want %v", tc.val, tc.def, got, tc.want)
			}
		})
	}
}

const cfgNoDN = `
components:
  - kind: Deployment
    namespace: default
    name: api
`

func TestRunConfigSync_DefaultsDisplayNameToName(t *testing.T) {
	st := testStoreWithMigrate(t)
	ctx := context.Background()

	_, code := runConfigSync(ctx, st, writeConfig(t, cfgNoDN), false, testLogger())
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}

	got, err := st.GetComponent(ctx, "Deployment/default/api")
	if err != nil {
		t.Fatalf("GetComponent: %v", err)
	}
	if got.DisplayName != "api" {
		t.Errorf("DisplayName = %q, want %q (defaulted to Name)", got.DisplayName, "api")
	}
}

func TestServeCmd_NoClusterShortCircuit(t *testing.T) {
	// Verify that --no-cluster causes serve setup to NOT attempt cluster
	// connectivity. We test this by running just the cluster-init step
	// in isolation via the helper exported for testing below.
	//
	// The full serveCmd starts an HTTP server, so we can't drive it
	// directly in a unit test. Instead, runClusterInit (extracted from
	// serveCmd in this PR) is the testable unit.

	logger := testLogger()
	ctx := context.Background()

	// --no-cluster set → returns (nil, nil) without touching kubeconfig.
	kc, err := runClusterInit(ctx, true, "", "", logger)
	if err != nil {
		t.Fatalf("--no-cluster: err = %v, want nil", err)
	}
	if kc != nil {
		t.Errorf("--no-cluster: kc = %v, want nil", kc)
	}

	// --no-cluster unset + bad --kubeconfig path → error.
	_, err = runClusterInit(ctx, false, "/does/not/exist-northwatch-test", "", logger)
	if err == nil {
		t.Fatal("bad kubeconfig: got nil err, want error")
	}
}

func TestEnvOrInt(t *testing.T) {
	cases := []struct {
		name string
		val  string
		set  bool
		def  int
		want int
	}{
		{name: "valid positive", val: "10", set: true, def: 5, want: 10},
		{name: "valid zero", val: "0", set: true, def: 5, want: 0},
		{name: "negative", val: "-3", set: true, def: 5, want: -3},
		{name: "garbage falls back", val: "five", set: true, def: 5, want: 5},
		{name: "empty falls back", val: "", set: true, def: 7, want: 7},
		{name: "unset falls back", val: "", set: false, def: 7, want: 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const key = "NW_TEST_ENVORINT"
			if tc.set {
				t.Setenv(key, tc.val)
			} else {
				t.Setenv(key, "ignored")
				if err := os.Unsetenv(key); err != nil {
					t.Fatalf("Unsetenv: %v", err)
				}
			}
			if got := envOrInt(key, tc.def); got != tc.want {
				t.Errorf("envOrInt(%q, %d) [val=%q] = %d, want %d", key, tc.def, tc.val, got, tc.want)
			}
		})
	}
}

func TestServeCmd_PollSecondsValidation(t *testing.T) {
	cases := []struct {
		name string
		val  string
		want int // expected exit code
	}{
		{name: "zero rejected", val: "0", want: 1},
		{name: "negative rejected", val: "-5", want: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code := serveCmd([]string{
				"--no-cluster",
				"--db", filepath.Join(t.TempDir(), "nw.db"),
				"--config", writeConfig(t, cfgAB),
				"--poll-seconds", tc.val,
			})
			if code != tc.want {
				t.Errorf("serveCmd exit = %d, want %d", code, tc.want)
			}
		})
	}
}

func TestServeCmd_RejectsExplicitEmptyAPIToken(t *testing.T) {
	cases := []struct {
		name   string
		envSet bool
		envVal string
		args   []string
	}{
		{name: "empty env", envSet: true, envVal: ""},
		{name: "empty flag", envSet: true, envVal: "valid-env-token-1234", args: []string{"--api-token", ""}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.envSet {
				t.Setenv("NORTHWATCH_API_TOKEN", tc.envVal)
			} else {
				t.Setenv("NORTHWATCH_API_TOKEN", "ignored")
				if err := os.Unsetenv("NORTHWATCH_API_TOKEN"); err != nil {
					t.Fatalf("Unsetenv: %v", err)
				}
			}

			args := []string{
				"--no-cluster",
				"--db", filepath.Join(t.TempDir(), "nw.db"),
				"--config", writeConfig(t, cfgAB),
				"--poll-seconds", "0",
			}
			args = append(args, tc.args...)

			logOut := captureStdout(t, func() {
				if code := serveCmd(args); code != 1 {
					t.Errorf("serveCmd exit = %d, want 1", code)
				}
			})
			if !strings.Contains(logOut, "api token configured empty") {
				t.Errorf("log output = %s, want api token configured empty", logOut)
			}
			if strings.Contains(logOut, "poll-seconds must be >= 1") {
				t.Errorf("log output = %s, token validation should run before poll validation", logOut)
			}
		})
	}
}

func TestServeCmd_OmittedAPITokenReachesLaterValidation(t *testing.T) {
	t.Setenv("NORTHWATCH_API_TOKEN", "ignored")
	if err := os.Unsetenv("NORTHWATCH_API_TOKEN"); err != nil {
		t.Fatalf("Unsetenv: %v", err)
	}

	logOut := captureStdout(t, func() {
		code := serveCmd([]string{
			"--no-cluster",
			"--db", filepath.Join(t.TempDir(), "nw.db"),
			"--config", writeConfig(t, cfgAB),
			"--poll-seconds", "0",
		})
		if code != 1 {
			t.Errorf("serveCmd exit = %d, want 1", code)
		}
	})
	if !strings.Contains(logOut, "poll-seconds must be >= 1") {
		t.Errorf("log output = %s, want poll-seconds validation", logOut)
	}
	if strings.Contains(logOut, "api token configured empty") {
		t.Errorf("log output = %s, omitted token should not be rejected as configured empty", logOut)
	}
}
