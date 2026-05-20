package status

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/northwatchlabs/northwatch/internal/component"
)

// MapHelmRelease returns the component status for a Flux HelmRelease.
// Honors the kstatus condition triplet (Stalled, Ready, Reconciling)
// with per-condition observedGeneration freshness. Precedence (first
// trusted match wins):
//
//   - Stalled=True (fresh)                       → down
//   - Ready=True (fresh)                         → operational
//   - Ready=False, reason=Progressing (fresh)    → degraded
//   - Ready=False, other reason (fresh)          → down
//   - Reconciling=True (fresh)                   → degraded
//   - otherwise                                  → unknown
//
// "Fresh" means observedGeneration == metadata.generation; stale
// conditions are ignored.
func MapHelmRelease(u *unstructured.Unstructured) component.Status {
	return mapFluxConditions(u)
}

// MapKustomization returns the component status for a Flux
// Kustomization (kustomize.toolkit.fluxcd.io/v1). Honors the kstatus
// condition triplet (Stalled, Ready, Reconciling) with per-condition
// observedGeneration freshness. Precedence (first trusted match
// wins):
//
//   - Stalled=True (fresh)                       → down
//   - Ready=True (fresh)                         → operational
//   - Ready=False, reason=Progressing (fresh)    → degraded
//   - Ready=False, other reason (fresh)          → down
//   - Reconciling=True (fresh)                   → degraded
//   - otherwise                                  → unknown
//
// "Fresh" means observedGeneration == metadata.generation; stale
// conditions are ignored.
func MapKustomization(u *unstructured.Unstructured) component.Status {
	return mapFluxConditions(u)
}

// asInt64 tolerantly decodes a value from an unstructured map. JSON
// deserialization into unstructured can produce either int64 or
// float64 for numeric fields depending on the code path, so accept
// both. Missing or unrecognized types decode as 0.
func asInt64(v interface{}) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case float64:
		return int64(x)
	case int:
		return int64(x)
	}
	return 0
}

// findCondition returns the status, reason, and observedGeneration of
// the first entry in status.conditions whose type matches condType.
// observedGenPresent indicates whether the observedGeneration field
// was explicitly set on the matched condition — callers must check
// this to avoid treating a missing field's zero value as a legitimate
// generation. Returns ("", "", 0, false, false) if no matching entry
// is present or the slice is malformed.
func findCondition(conds []interface{}, condType string) (status, reason string, observedGen int64, observedGenPresent bool, found bool) {
	for _, c := range conds {
		m, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		t, _ := m["type"].(string)
		if t != condType {
			continue
		}
		s, _ := m["status"].(string)
		r, _ := m["reason"].(string)
		ogv, ogPresent := m["observedGeneration"]
		og := asInt64(ogv)
		return s, r, og, ogPresent, true
	}
	return "", "", 0, false, false
}

// mapFluxConditions is the shared kstatus-aware mapper used by
// MapHelmRelease and MapKustomization. Each of Stalled, Ready, and
// Reconciling is checked for freshness independently — a condition
// is trusted iff its observedGeneration matches the resource's
// metadata.generation. Stale conditions are ignored as if absent.
// See MapHelmRelease for the full precedence table.
func mapFluxConditions(u *unstructured.Unstructured) component.Status {
	conds, found, err := unstructured.NestedSlice(u.Object, "status", "conditions")
	if err != nil || !found {
		return component.StatusUnknown
	}

	gen := u.GetGeneration()

	if s, _, og, ogP, ok := findCondition(conds, "Stalled"); ok && ogP && og == gen && s == "True" {
		return component.StatusDown
	}

	readyStatus, readyReason, readyOG, readyOGP, hasReady := findCondition(conds, "Ready")
	if hasReady && readyOGP && readyOG == gen {
		switch readyStatus {
		case "True":
			return component.StatusOperational
		case "False":
			if readyReason == "Progressing" {
				return component.StatusDegraded
			}
			return component.StatusDown
		}
		// Ready=Unknown or unrecognized: fall through to Reconciling.
	}

	if s, _, og, ogP, ok := findCondition(conds, "Reconciling"); ok && ogP && og == gen && s == "True" {
		return component.StatusDegraded
	}

	return component.StatusUnknown
}

// MapApplication returns the component status for an ArgoCD
// Application based on status.health.status. The second return is
// false only for Suspended — preserve whatever the store already
// shows (intentional operator pause; no write).
//
//   - Healthy                  → operational
//   - Progressing              → degraded
//   - Degraded, Missing        → down
//   - Unknown                  → unknown (explicit; "we lost the signal")
//   - Suspended                → ("", false) preserve
//   - field missing or empty   → unknown
//   - unrecognized (future)    → unknown
func MapApplication(u *unstructured.Unstructured) (component.Status, bool) {
	health, found, err := unstructured.NestedString(u.Object, "status", "health", "status")
	if err != nil || !found || health == "" {
		return component.StatusUnknown, true
	}
	switch health {
	case "Healthy":
		return component.StatusOperational, true
	case "Progressing":
		return component.StatusDegraded, true
	case "Degraded", "Missing":
		return component.StatusDown, true
	case "Unknown":
		return component.StatusUnknown, true
	case "Suspended":
		return "", false
	default:
		return component.StatusUnknown, true
	}
}

// MapDeployment returns the component status for an apps/v1
// Deployment. Evaluation order:
//
//  1. Check status.conditions[type=Progressing]:
//     - True, reason=NewReplicaSetAvailable, ready==desired → operational
//     - False, reason=ProgressDeadlineExceeded             → down
//     - any other Progressing state                        → fall through
//  2. Replica-count fallback:
//     - ready == 0                                         → down
//     - ready < desired                                    → degraded
//     - otherwise                                          → operational
//
// spec.Replicas defaults to 1 when nil (the k8s default).
//
// There is no "preserve previous" return. The Debouncer is the only
// mechanism that suppresses transient downward transitions. A stuck
// rollout (Progressing=True/ReplicaSetUpdated with ready < desired
// for longer than the debounce window) is the user-facing signal,
// not a state to be hidden until ProgressDeadlineExceeded fires.
func MapDeployment(d *appsv1.Deployment) component.Status {
	desired := int32(1)
	if d.Spec.Replicas != nil {
		desired = *d.Spec.Replicas
	}
	ready := d.Status.ReadyReplicas

	for _, c := range d.Status.Conditions {
		if c.Type != appsv1.DeploymentProgressing {
			continue
		}
		switch c.Status {
		case corev1.ConditionTrue:
			if c.Reason == "NewReplicaSetAvailable" && ready >= desired && desired > 0 {
				return component.StatusOperational
			}
		case corev1.ConditionFalse:
			if c.Reason == "ProgressDeadlineExceeded" {
				return component.StatusDown
			}
		}
		break // only one Progressing condition expected
	}

	switch {
	case ready == 0:
		return component.StatusDown
	case ready < desired:
		return component.StatusDegraded
	default:
		return component.StatusOperational
	}
}
