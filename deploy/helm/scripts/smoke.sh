#!/usr/bin/env bash
#
# End-to-end Helm chart smoke. Builds the NorthWatch image with the
# chart's appVersion as its tag, loads it into kind, installs the
# chart with NO image overrides (so the chart's default image
# reference is what gets exercised), applies a sample Deployment,
# then polls /api/components until the watcher reports the component
# operational.
#
# Inputs (env vars):
#   HELM          path to the helm binary (default: "helm" on PATH)
#   KIND_CLUSTER  kind cluster name (default: northwatch-smoke)
#   KEEP_CLUSTER  if set, skip cluster teardown on exit
#
# Exit code is the success of the smoke. Cluster + port-forward
# teardown happens on any exit path via trap.
set -euo pipefail

HELM="${HELM:-helm}"
KIND_CLUSTER="${KIND_CLUSTER:-northwatch-smoke}"
CHART_DIR="$(cd "$(dirname "$0")/.." && pwd)/northwatch"
VALUES_FILE="$(cd "$(dirname "$0")/.." && pwd)/ci/smoke-values.yaml"
SAMPLE_WORKLOAD="$(cd "$(dirname "$0")/../../.." && pwd)/examples/basic/sample-deployment.yaml"
RELEASE="nw"
NAMESPACE="northwatch"
PF_PID=""

cleanup() {
  set +e
  [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null
  if [ -z "${KEEP_CLUSTER:-}" ]; then
    kind delete cluster --name "$KIND_CLUSTER" >/dev/null 2>&1
  fi
}
trap cleanup EXIT

echo "==> Helm version"
"$HELM" version --short

echo "==> Resolve chart appVersion"
APP_VERSION=$("$HELM" show chart "$CHART_DIR" \
  | awk '/^appVersion:/ {gsub(/"/, "", $2); print $2; exit}')
if [ -z "$APP_VERSION" ]; then
  echo "could not resolve appVersion from chart" >&2
  exit 1
fi
IMAGE="ghcr.io/northwatchlabs/northwatch:${APP_VERSION}"
echo "appVersion=$APP_VERSION image=$IMAGE"

echo "==> Create kind cluster $KIND_CLUSTER"
kind create cluster --name "$KIND_CLUSTER" --wait 60s

echo "==> Build NorthWatch image as $IMAGE"
docker build -f deploy/docker/Dockerfile -t "$IMAGE" .

echo "==> Load image into kind"
kind load docker-image "$IMAGE" --name "$KIND_CLUSTER"

echo "==> Apply sample workload"
kubectl apply -f "$SAMPLE_WORKLOAD"

echo "==> helm install (no image overrides)"
kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -
"$HELM" install "$RELEASE" "$CHART_DIR" \
  --namespace "$NAMESPACE" \
  --values "$VALUES_FILE" \
  --wait --timeout 120s

echo "==> Wait for both Deployments Available"
kubectl -n "$NAMESPACE" wait deploy/"${RELEASE}-northwatch" \
  --for=condition=available --timeout=60s
kubectl wait deploy/api-gateway --for=condition=available --timeout=60s

echo "==> Port-forward and curl /healthz"
kubectl -n "$NAMESPACE" port-forward "svc/${RELEASE}-northwatch" 18080:8080 \
  >/dev/null 2>&1 &
PF_PID=$!
# Give the port-forward a moment to bind.
for _ in 1 2 3 4 5; do
  sleep 1
  if curl -fsS http://localhost:18080/healthz >/dev/null 2>&1; then
    break
  fi
done
curl -fsS http://localhost:18080/healthz

echo "==> Poll /api/components until api-gateway is operational"
deadline=$(( $(date +%s) + 60 ))
while :; do
  if curl -fsS http://localhost:18080/api/components \
    | jq -e '.[] | select(.name=="api-gateway" and .status=="operational")' \
    >/dev/null; then
    echo "    api-gateway operational"
    break
  fi
  if [ "$(date +%s)" -ge "$deadline" ]; then
    echo "    timed out waiting for api-gateway operational" >&2
    curl -fsS http://localhost:18080/api/components || true
    exit 1
  fi
  sleep 2
done

echo "==> helm uninstall + assert RBAC cleanup"
# --wait blocks until all release resources (including cluster-scoped
# RBAC) are actually deleted, not just marked for deletion. Without it
# the kubectl get checks below race the async delete.
"$HELM" uninstall "$RELEASE" --namespace "$NAMESPACE" --wait --timeout 60s
if kubectl get clusterrole "${RELEASE}-northwatch" >/dev/null 2>&1; then
  echo "ClusterRole survived uninstall" >&2
  exit 1
fi
if kubectl get clusterrolebinding "${RELEASE}-northwatch" >/dev/null 2>&1; then
  echo "ClusterRoleBinding survived uninstall" >&2
  exit 1
fi

echo "==> Smoke OK"
