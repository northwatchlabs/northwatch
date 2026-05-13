package component_test

import (
	"testing"

	"github.com/northwatchlabs/northwatch/internal/component"
)

func TestComponentIDFormatsKindSlashNamespaceSlashName(t *testing.T) {
	c := component.Component{
		Kind:      "Deployment",
		Namespace: "default",
		Name:      "northwatch",
	}
	got := c.ID()
	want := "Deployment/default/northwatch"
	if got != want {
		t.Fatalf("ID() = %q, want %q", got, want)
	}
}

func TestStatusConstantsHaveExpectedValues(t *testing.T) {
	cases := map[component.Status]string{
		component.StatusUnknown:     "unknown",
		component.StatusOperational: "operational",
		component.StatusDegraded:    "degraded",
		component.StatusDown:        "down",
	}
	for got, want := range cases {
		if string(got) != want {
			t.Errorf("Status const %q, want %q", string(got), want)
		}
	}
}
