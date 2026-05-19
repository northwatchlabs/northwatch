package status

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/northwatchlabs/northwatch/internal/component"
)

func TestMapHelmRelease(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		hr   *unstructured.Unstructured
		want component.Status
	}{
		{
			name: "Ready=True maps to operational",
			hr:   helmReleaseWithReady("True", ""),
			want: component.StatusOperational,
		},
		{
			name: "Ready=False, reason=Progressing maps to degraded",
			hr:   helmReleaseWithReady("False", "Progressing"),
			want: component.StatusDegraded,
		},
		{
			name: "Ready=False, other reason maps to down",
			hr:   helmReleaseWithReady("False", "InstallFailed"),
			want: component.StatusDown,
		},
		{
			name: "Ready missing maps to unknown",
			hr:   newHelmRelease(map[string]interface{}{"status": map[string]interface{}{}}),
			want: component.StatusUnknown,
		},
		{
			name: "Ready=Unknown maps to unknown",
			hr:   helmReleaseWithReady("Unknown", ""),
			want: component.StatusUnknown,
		},
		{
			name: "no status.conditions at all maps to unknown",
			hr:   newHelmRelease(nil),
			want: component.StatusUnknown,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := MapHelmRelease(tc.hr)
			if got != tc.want {
				t.Fatalf("MapHelmRelease = %q, want %q", got, tc.want)
			}
		})
	}
}

func helmReleaseWithReady(readyStatus, reason string) *unstructured.Unstructured {
	cond := map[string]interface{}{
		"type":   "Ready",
		"status": readyStatus,
	}
	if reason != "" {
		cond["reason"] = reason
	}
	return newHelmRelease(map[string]interface{}{
		"status": map[string]interface{}{
			"conditions": []interface{}{cond},
		},
	})
}

func newHelmRelease(obj map[string]interface{}) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "helm.toolkit.fluxcd.io",
		Version: "v2",
		Kind:    "HelmRelease",
	})
	u.SetNamespace("default")
	u.SetName("test")
	for k, v := range obj {
		u.Object[k] = v
	}
	return u
}

func TestMapApplication(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		app       *unstructured.Unstructured
		wantSt    component.Status
		wantWrite bool
	}{
		{"Healthy → operational", appWithHealth("Healthy"), component.StatusOperational, true},
		{"Progressing → degraded", appWithHealth("Progressing"), component.StatusDegraded, true},
		{"Degraded → down", appWithHealth("Degraded"), component.StatusDown, true},
		{"Missing → down", appWithHealth("Missing"), component.StatusDown, true},
		{"Unknown → unknown (explicit)", appWithHealth("Unknown"), component.StatusUnknown, true},
		{"Suspended → preserve previous", appWithHealth("Suspended"), "", false},
		{"unrecognized → unknown", appWithHealth("Pancakes"), component.StatusUnknown, true},
		{"missing field → unknown", newApp(map[string]interface{}{"status": map[string]interface{}{}}), component.StatusUnknown, true},
		{"empty health string → unknown", appWithHealth(""), component.StatusUnknown, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotSt, gotWrite := MapApplication(tc.app)
			if gotWrite != tc.wantWrite {
				t.Fatalf("write = %v, want %v", gotWrite, tc.wantWrite)
			}
			if gotWrite && gotSt != tc.wantSt {
				t.Fatalf("status = %q, want %q", gotSt, tc.wantSt)
			}
		})
	}
}

func appWithHealth(h string) *unstructured.Unstructured {
	status := map[string]interface{}{}
	if h != "" {
		status["health"] = map[string]interface{}{"status": h}
	}
	return newApp(map[string]interface{}{"status": status})
}

func newApp(obj map[string]interface{}) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "argoproj.io",
		Version: "v1alpha1",
		Kind:    "Application",
	})
	u.SetNamespace("default")
	u.SetName("test")
	for k, v := range obj {
		u.Object[k] = v
	}
	return u
}

func TestMapDeployment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		d    *appsv1.Deployment
		want component.Status
	}{
		{
			"Progressing=True NewReplicaSetAvailable, ready==desired → operational",
			deployWithProgressing(corev1.ConditionTrue, "NewReplicaSetAvailable", 3, 3),
			component.StatusOperational,
		},
		{
			"Progressing=False ProgressDeadlineExceeded → down",
			deployWithProgressing(corev1.ConditionFalse, "ProgressDeadlineExceeded", 3, 0),
			component.StatusDown,
		},
		{
			"Progressing=True ReplicaSetUpdated, ready < desired → replica fallback degraded",
			deployWithProgressing(corev1.ConditionTrue, "ReplicaSetUpdated", 3, 2),
			component.StatusDegraded,
		},
		{
			"Progressing=True NewReplicaSetAvailable, ready < desired → replica fallback degraded",
			deployWithProgressing(corev1.ConditionTrue, "NewReplicaSetAvailable", 3, 2),
			component.StatusDegraded,
		},
		{
			"Progressing=True NewReplicaSetCreated → replica fallback (0/3 = down)",
			deployWithProgressing(corev1.ConditionTrue, "NewReplicaSetCreated", 3, 0),
			component.StatusDown,
		},
		{
			"no Progressing condition, ready==desired → operational",
			deployWithReplicas(3, 3),
			component.StatusOperational,
		},
		{
			"no Progressing condition, ready < desired → degraded",
			deployWithReplicas(3, 1),
			component.StatusDegraded,
		},
		{
			"no Progressing condition, ready=0 → down",
			deployWithReplicas(3, 0),
			component.StatusDown,
		},
		{
			"spec.Replicas nil defaults to 1, ready=1 → operational",
			deployWithNilSpecReady(1),
			component.StatusOperational,
		},
		{
			"intentional scale-to-zero (desired=0, ready=0) → down",
			deployWithReplicas(0, 0),
			component.StatusDown,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := MapDeployment(tc.d)
			if got != tc.want {
				t.Fatalf("MapDeployment = %q, want %q", got, tc.want)
			}
		})
	}
}

func deployWithProgressing(s corev1.ConditionStatus, reason string, desired, ready int32) *appsv1.Deployment {
	d := deployWithReplicas(desired, ready)
	d.Status.Conditions = []appsv1.DeploymentCondition{
		{Type: appsv1.DeploymentProgressing, Status: s, Reason: reason},
	}
	return d
}

func deployWithReplicas(desired, ready int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		Spec:   appsv1.DeploymentSpec{Replicas: &desired},
		Status: appsv1.DeploymentStatus{ReadyReplicas: ready},
	}
}

func deployWithNilSpecReady(ready int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		Spec:   appsv1.DeploymentSpec{Replicas: nil},
		Status: appsv1.DeploymentStatus{ReadyReplicas: ready},
	}
}
