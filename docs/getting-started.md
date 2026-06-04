# Getting Started

This walkthrough runs NorthWatch against a local kind cluster and the
sample `Deployment` from the repository.

## Prerequisites

- Docker
- `mise`
- a checkout of `github.com/northwatchlabs/northwatch`

Install the pinned project tools:

```sh
mise trust
mise install
```

## Run the demo

Create a local cluster and the workload NorthWatch will watch:

```sh
kind create cluster --name nw-demo
kubectl --context kind-nw-demo apply -f examples/basic/sample-deployment.yaml
```

Install NorthWatch with the in-tree chart:

```sh
kubectl --context kind-nw-demo create namespace northwatch
helm --kube-context kind-nw-demo install nw deploy/helm/northwatch \
  --namespace northwatch \
  --set image.tag=0.1.0 \
  --set-file config=examples/basic/northwatch.yaml
```

Open the status page:

```sh
kubectl --context kind-nw-demo -n northwatch port-forward svc/nw-northwatch 8080:8080
```

Visit <http://localhost:8080>. The `API Gateway` component should move
to `operational` after the sample deployment is available.

## Exercise a status transition

Scale the deployment to zero:

```sh
kubectl --context kind-nw-demo -n default scale deploy/api-gateway --replicas=0
```

After the debounce window, the component moves to `down`. Scale it back:

```sh
kubectl --context kind-nw-demo -n default scale deploy/api-gateway --replicas=1
```

The component returns to `operational` when the rollout completes.

## Open an incident

The Helm chart creates a bearer token for write endpoints. Retrieve it
and create an incident:

```sh
TOKEN=$(kubectl --context kind-nw-demo -n northwatch get secret \
  nw-northwatch-api-token -o jsonpath='{.data.token}' | base64 --decode)

curl -fsS -X POST http://localhost:8080/incidents \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"title":"Investigating elevated 5xx errors","component":"Deployment/default/api-gateway"}'
```

The public page shows an incident banner while the incident is active.

## Clean up

```sh
kind delete cluster --name nw-demo
```
