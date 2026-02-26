# Local Kind Cluster Setup and Teardown for EPP

This guide provides a baseline reference manual for the local testing lifecycle on the Endpoint Picker Proxy (EPP). It is suitable for human or automated development workflows.

## Prerequisites
Prerequisites: `go`, `docker`, `kind`, `kubectl`, `helm`, and Google Cloud Application Default Credentials.

## Cluster Lifecycle Management

### Creation
Use this standard definition to instantiate a default `inference-e2e` cluster.

```bash
cat <<EOF > /tmp/kind-config.yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
EOF
kind create cluster --name inference-e2e --config /tmp/kind-config.yaml
```

Wait for node availability via `kubectl wait`.

### Teardown
To clear your namespace and recover local docker resources:

```bash
kind delete cluster --name inference-e2e
```

## EPP Development Deployment

### 1. Build and Load Docker Images
Compile your local additions and directly sideline into the node control-plane.

```bash
make docker-build-epp
kind load docker-image epp:latest --name inference-e2e
```

### 2. Apply Custom Resource Definitions (CRDs)
Register necessary interfaces.

```bash
kubectl apply -k config/crd --context kind-inference-e2e
```

### 3. Deploy Standalone EPP with vLLM Simulators
Inject a locally resolved image specification over the standard helm tree.

```bash
cat << 'EOF' > values-override-inference.yaml
inferenceExtension:
  image:
    tag: latest
    pullPolicy: Never
inferencePool:
  modelServers:
    matchLabels:
      app: vllm-llama3-8b-instruct
EOF

helm install standalone-test-inference ./config/charts/standalone -f values-override-inference.yaml --kube-context kind-inference-e2e
```

### 4. Observe Ext-Proc Sidecar Routing
Check runtime verification logs and test via proxy requests over cluster curl contexts.

```bash
kubectl logs -l app.kubernetes.io/name=standalone-test-inference-epp -c epp --context kind-inference-e2e | grep "Admission passed"
```
