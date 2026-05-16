package incident_test

import (
	"testing"

	"github.com/northwatchlabs/northwatch/internal/incident"
)

func TestNewIDReturnsDistinctULIDs(t *testing.T) {
	a := incident.NewID()
	b := incident.NewID()
	if a == b {
		t.Fatalf("NewID returned duplicate value: %s", a)
	}
	if len(a) != 26 {
		t.Fatalf("NewID length = %d, want 26 (ULID)", len(a))
	}
}
