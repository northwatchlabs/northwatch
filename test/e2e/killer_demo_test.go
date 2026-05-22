//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestKillerDemo(t *testing.T) {
	e := envFromOS()
	requireContext(t, e)
	diag := newDiag(t, e)

	// Step 1: Apply sample workload, wait Available.
	workloadManifest := "testdata/sample-deployment.yaml"
	if out, err := kubectl(t, e, "apply", "-f", workloadManifest); err != nil {
		diag.fatal(t, "apply sample workload: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		if out, err := kubectl(t, e, "delete", "-f", workloadManifest, "--ignore-not-found"); err != nil {
			t.Logf("cleanup: delete sample workload: %v\n%s", err, out)
		}
	})
	if out, err := kubectlT(t, e, 90*time.Second, "-n", workloadNamespace,
		"wait", "deploy/api-gateway", "--for=condition=available", "--timeout=60s"); err != nil {
		diag.fatal(t, "wait sample workload Available: %v\n%s", err, out)
	}

	// Step 2: Pre-install reset, then install NorthWatch.
	preInstallReset(t, e)
	t.Cleanup(func() { teardownNorthwatch(t, e) })

	chartPath := "../../deploy/helm/northwatch"
	valuesPath := "../../deploy/helm/ci/e2e-values.yaml"
	if out, err := helm(t, e, 150*time.Second,
		"install", releaseName, chartPath,
		"--namespace", releaseNamespace, "--create-namespace",
		"--set", "auth.token="+e.token,
		"--values", valuesPath,
		"--wait", "--timeout", "120s"); err != nil {
		diag.fatal(t, "helm install: %v\n%s", err, out)
	}

	// Step 3: Port-forward.
	stopPF := portForward(t, e)
	t.Cleanup(stopPF)

	// Smoke check.
	if code, body := httpGet(t, e.apiBase+"/healthz"); code != 200 {
		diag.fatal(t, "/healthz returned %d: %s", code, body)
	}

	// Step 4: Wait operational, capture component ID.
	var apiGW apiComponent
	pollUntil(t, diag, "api-gateway operational", 30*time.Second, 1*time.Second, func() error {
		comps, err := fetchComponents(t, e, diag)
		if err != nil {
			return err
		}
		c, err := findAPIGateway(comps)
		if err != nil {
			return err
		}
		if c.Status != "operational" {
			return fmt.Errorf("status=%q want operational", c.Status)
		}
		apiGW = c
		return nil
	})
	if apiGW.ID != componentID {
		diag.fatal(t, "component ID = %q, want %q", apiGW.ID, componentID)
	}

	// Step 5: Scale to zero, wait for down (120s past debounce).
	if out, err := kubectl(t, e, "-n", workloadNamespace,
		"scale", "deploy/api-gateway", "--replicas=0"); err != nil {
		diag.fatal(t, "scale to 0: %v\n%s", err, out)
	}
	pollUntil(t, diag, "api-gateway down", 120*time.Second, 1*time.Second, func() error {
		comps, err := fetchComponents(t, e, diag)
		if err != nil {
			return err
		}
		c, err := findAPIGateway(comps)
		if err != nil {
			return err
		}
		if c.Status != "down" {
			return fmt.Errorf("status=%q want down", c.Status)
		}
		return nil
	})

	// Step 6: Create incident, capture id.
	body := fmt.Sprintf(`{"component":%q,"title":%q}`, componentID, incidentTitle)
	code, respBody := httpPostJSON(t, e.apiBase+"/incidents", e.token, body)
	if code != 201 {
		diag.fatal(t, "POST /incidents: status %d body=%s", code, respBody)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := jsonUnmarshal([]byte(respBody), &created); err != nil {
		diag.fatal(t, "decode POST /incidents response: %v body=%s", err, respBody)
	}
	if created.ID == "" {
		diag.fatal(t, "POST /incidents: empty id in body=%s", respBody)
	}

	// Step 7: Assert incident banner present.
	code, page := httpGet(t, e.apiBase+"/")
	if code != 200 {
		diag.fatal(t, "GET / : status %d", code)
	}
	if !strings.Contains(page, incidentTitle) {
		diag.fatal(t, "GET /: body missing incident title %q\nbody:\n%s", incidentTitle, page)
	}
	if !strings.Contains(page, "text-red-900") {
		diag.fatal(t, "GET /: body missing red-banner class text-red-900\nbody:\n%s", page)
	}

	// Step 8: Resolve incident.
	code, respBody = httpPostJSON(t, e.apiBase+"/incidents/"+created.ID+"/resolve", e.token, "")
	if code != 200 {
		diag.fatal(t, "POST resolve: status %d body=%s", code, respBody)
	}

	// Step 9: Restore replicas, wait operational again.
	if out, err := kubectl(t, e, "-n", workloadNamespace,
		"scale", "deploy/api-gateway", "--replicas=3"); err != nil {
		diag.fatal(t, "scale to 3: %v\n%s", err, out)
	}
	pollUntil(t, diag, "api-gateway operational (recovery)", 60*time.Second, 1*time.Second, func() error {
		comps, err := fetchComponents(t, e, diag)
		if err != nil {
			return err
		}
		c, err := findAPIGateway(comps)
		if err != nil {
			return err
		}
		if c.Status != "operational" {
			return fmt.Errorf("status=%q want operational", c.Status)
		}
		return nil
	})

	// Step 10: Assert banner cleared.
	code, page = httpGet(t, e.apiBase+"/")
	if code != 200 {
		diag.fatal(t, "GET / (final): status %d", code)
	}
	if !strings.Contains(page, "All Systems Operational") {
		diag.fatal(t, "GET /: missing 'All Systems Operational'\nbody:\n%s", page)
	}
	for _, banned := range []string{incidentTitle, "text-red-900", "Some Systems Degraded"} {
		if strings.Contains(page, banned) {
			diag.fatal(t, "GET /: should not contain %q after resolve+recovery\nbody:\n%s", banned, page)
		}
	}

	// Assert the incident is no longer active.
	code, incBody := httpGet(t, e.apiBase+"/api/incidents")
	diag.lastIncidents = incBody
	if code != 200 {
		diag.fatal(t, "GET /api/incidents: status %d", code)
	}
	if strings.Contains(incBody, created.ID) {
		diag.fatal(t, "GET /api/incidents: resolved id %q still active\nbody:\n%s", created.ID, incBody)
	}
}
