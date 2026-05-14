package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/northwatchlabs/northwatch/internal/config"
)

const validYAML = `
components:
  - kind: Deployment
    namespace: default
    name: api-gateway
    displayName: "API Gateway"
  - kind: HelmRelease
    namespace: flux-system
    name: cert-manager
    displayName: "Cert Manager"
  - kind: Application
    namespace: argocd
    name: payments
    displayName: "Payments Service"
`

// writeTempConfig writes body to a fresh tempfile and returns its
// path. The file is cleaned up by t.TempDir's cleanup.
func writeTempConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "northwatch.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestLoad_Valid(t *testing.T) {
	path := writeTempConfig(t, validYAML)
	f, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(f.Components) != 3 {
		t.Fatalf("len(Components) = %d, want 3", len(f.Components))
	}
	if got, want := f.Components[0].Kind, "Deployment"; got != want {
		t.Errorf("[0].Kind = %q, want %q", got, want)
	}
	if got, want := f.Components[1].DisplayName, "Cert Manager"; got != want {
		t.Errorf("[1].DisplayName = %q, want %q", got, want)
	}
	if got, want := f.Components[2].Name, "payments"; got != want {
		t.Errorf("[2].Name = %q, want %q", got, want)
	}
}

func TestLoad_MissingFile(t *testing.T) {
	_, err := config.Load("/does/not/exist/northwatch.yaml")
	if err == nil {
		t.Fatal("Load err = nil, want error")
	}
	if !strings.Contains(err.Error(), "/does/not/exist/northwatch.yaml") {
		t.Errorf("err = %q, want it to mention the path", err)
	}
}

func TestLoad_MalformedYAML(t *testing.T) {
	path := writeTempConfig(t, "components: ][\n")
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("Load err = nil, want parse error")
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Errorf("err = %q, want it to mention 'parse'", err)
	}
}

// asValidationError unwraps to *config.ValidationError or fails.
func asValidationError(t *testing.T, err error) *config.ValidationError {
	t.Helper()
	var ve *config.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *config.ValidationError", err)
	}
	return ve
}

func load(t *testing.T, body string) error {
	t.Helper()
	path := writeTempConfig(t, body)
	_, err := config.Load(path)
	return err
}

func TestValidate_RequiredFields(t *testing.T) {
	body := `
components:
  - kind: ""
    namespace: ""
    name: ""
`
	ve := asValidationError(t, load(t, body))
	gotMsg := ve.Error()
	for _, want := range []string{
		"kind is required",
		"namespace is required",
		"name is required",
	} {
		if !strings.Contains(gotMsg, want) {
			t.Errorf("error missing %q\nfull: %s", want, gotMsg)
		}
	}
}

func TestValidate_KindAllowlist(t *testing.T) {
	body := `
components:
  - kind: Pod
    namespace: default
    name: web
`
	ve := asValidationError(t, load(t, body))
	if !strings.Contains(ve.Error(), `kind "Pod" is not one of`) {
		t.Errorf("error missing allowlist mention: %s", ve.Error())
	}
}

func TestValidate_SlashInField(t *testing.T) {
	body := `
components:
  - kind: Deployment
    namespace: default
    name: bad/name
`
	ve := asValidationError(t, load(t, body))
	if !strings.Contains(ve.Error(), `name must not contain '/'`) {
		t.Errorf("error missing slash rule: %s", ve.Error())
	}
}

func TestValidate_DuplicateID(t *testing.T) {
	body := `
components:
  - kind: Deployment
    namespace: default
    name: a
  - kind: Deployment
    namespace: default
    name: b
  - kind: Deployment
    namespace: default
    name: a
`
	ve := asValidationError(t, load(t, body))
	got := ve.Error()
	if !strings.Contains(got, `duplicate component id "Deployment/default/a"`) {
		t.Errorf("error missing dup id: %s", got)
	}
	if !strings.Contains(got, "[0, 2]") {
		t.Errorf("error missing indices [0, 2]: %s", got)
	}
}

func TestValidate_NoComponents(t *testing.T) {
	body := `components: []` + "\n"
	ve := asValidationError(t, load(t, body))
	if !strings.Contains(ve.Error(), "no components defined") {
		t.Errorf("error missing empty rule: %s", ve.Error())
	}
}

func TestValidate_AggregatesAll(t *testing.T) {
	body := `
components:
  - kind: ""
    namespace: default
    name: a
  - kind: Pod
    namespace: default
    name: b
  - kind: Deployment
    namespace: default
    name: c/d
`
	ve := asValidationError(t, load(t, body))
	if len(ve.Errs) != 3 {
		t.Fatalf("len(Errs) = %d, want 3 (one per row); errs: %v",
			len(ve.Errs), ve.Errs)
	}
}

func TestValidate_DuplicateIDOrderingDeterministic(t *testing.T) {
	// Two distinct dup groups: ensure the first-seen group is
	// reported first regardless of map iteration order. Run a few
	// times to make randomized ordering visible if it leaks back in.
	body := `
components:
  - kind: Deployment
    namespace: default
    name: a
  - kind: Deployment
    namespace: default
    name: z
  - kind: Deployment
    namespace: default
    name: a
  - kind: Deployment
    namespace: default
    name: z
`
	for i := 0; i < 20; i++ {
		ve := asValidationError(t, load(t, body))
		msg := ve.Error()
		aIdx := strings.Index(msg, `"Deployment/default/a"`)
		zIdx := strings.Index(msg, `"Deployment/default/z"`)
		if aIdx == -1 || zIdx == -1 {
			t.Fatalf("missing dup ids in: %s", msg)
		}
		if aIdx > zIdx {
			t.Fatalf("expected 'a' dup before 'z' dup (first-seen order); got msg:\n%s", msg)
		}
	}
}
