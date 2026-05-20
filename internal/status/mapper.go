package status

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/northwatchlabs/northwatch/internal/component"
)

// MapHelmRelease returns the component status for a Flux HelmRelease
// based on status.conditions[type=Ready].
//
//   - Ready=True                       → operational
//   - Ready=False, reason=Progressing  → degraded
//   - Ready=False, other reason        → down
//   - Ready missing/Unknown            → unknown
func MapHelmRelease(u *unstructured.Unstructured) component.Status {
	return mapReadyCondition(u)
}

// MapKustomization returns the component status for a Flux
// Kustomization (kustomize.toolkit.fluxcd.io/v1) based on
// status.conditions[type=Ready].
//
//   - Ready=True                       → operational
//   - Ready=False, reason=Progressing  → degraded
//   - Ready=False, other reason        → down
//   - Ready missing/Unknown            → unknown
//
// kustomize-controller also surfaces separate Reconciling and Stalled
// conditions (kstatus). Inspecting those is tracked separately (#57)
// and must land symmetrically in MapHelmRelease and MapKustomization.
func MapKustomization(u *unstructured.Unstructured) component.Status {
	return mapReadyCondition(u)
}

// mapReadyCondition is the shared Ready-condition mapper used by
// MapHelmRelease and MapKustomization. Both Flux controllers report
// reconcile state through status.conditions[type=Ready] with the
// same shape: status ∈ {True,False,Unknown}, reason ∈ {Progressing,
// <controller-specific failure reasons>...}. Keep the two exported
// functions as thin delegates so a future kstatus-aware mapper (see
// #57) can swap the implementation in one place.
func mapReadyCondition(u *unstructured.Unstructured) component.Status {
	conds, found, err := unstructured.NestedSlice(u.Object, "status", "conditions")
	if err != nil || !found {
		return component.StatusUnknown
	}
	for _, c := range conds {
		m, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if t, _ := m["type"].(string); t != "Ready" {
			continue
		}
		s, _ := m["status"].(string)
		switch s {
		case "True":
			return component.StatusOperational
		case "False":
			reason, _ := m["reason"].(string)
			if reason == "Progressing" {
				return component.StatusDegraded
			}
			return component.StatusDown
		default:
			return component.StatusUnknown
		}
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
