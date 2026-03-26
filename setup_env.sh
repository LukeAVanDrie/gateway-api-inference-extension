#!/bin/bash
set -e # Exit immediately if a command exits with a non-zero status

# --- Color formatting for terminal output ---
GREEN='\033[1;32m'
BLUE='\033[1;34m'
YELLOW='\033[1;33m'
RED='\033[1;31m'
NC='\033[0m' # No Color

function step() {
    echo -e "\n${BLUE}▶ $1${NC}"
}

# --- 0. Pre-Flight Checks ---
export PATH=$PATH:$HOME/go/bin

for cmd in kind kubectl helm make docker; do
    if ! command -v $cmd &> /dev/null; then
        echo -e "${RED}FATAL: '$cmd' is required but not installed or not in PATH.${NC}"
        exit 1
    fi
done

step "1. Creating fresh kind cluster (inference-demo)..."
# We only use || true here because 'kind delete' fails if the cluster doesn't exist yet
kind delete cluster --name inference-demo 2>/dev/null || true
kind create cluster --name inference-demo


step "2. Deploying Simulated Model Server Backend (vLLM)"
export MODEL_SERVER=vllm
export INFERENCE_POOL_NAME=vllm-qwen3-32b
export MODEL_NAME=Qwen/Qwen3-32B

# Ensure the local file exists before applying
if [ ! -f "config/manifests/vllm/sim-deployment.yaml" ]; then
    echo -e "\n${RED}FATAL: config/manifests/vllm/sim-deployment.yaml not found! Make sure you run this from the repo root.${NC}"
    exit 1
fi
kubectl apply -f config/manifests/vllm/sim-deployment.yaml


step "3. Installing Gateway API and Inference Extension CRDs"
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.2.0/standard-install.yaml
kubectl apply -k config/crd


step "4. Installing Agentgateway"
AGW_VERSION=v1.0.0

# OCI charts don't require 'helm repo add', just direct upgrade -i
helm upgrade -i --create-namespace --namespace agentgateway-system --version $AGW_VERSION \
  agentgateway-crds oci://cr.agentgateway.dev/charts/agentgateway-crds

# Wait for CRDs to be fully established in the API server
kubectl wait --for=condition=Established crd/gateways.gateway.networking.k8s.io --timeout=60s

helm upgrade -i --namespace agentgateway-system --version $AGW_VERSION \
  agentgateway oci://cr.agentgateway.dev/charts/agentgateway \
  --set inferenceExtension.enabled=true

kubectl apply -f config/manifests/gateway/agentgateway/gateway.yaml

echo "Waiting for Inference Gateway to be Programmed..."
# Give the controller a moment to see the object before waiting
sleep 3
kubectl wait --for=condition=Programmed --timeout=90s gateway/inference-gateway -n default


step "5. Generating Flow Control Configuration (epp-values.yaml)"
cat <<EOF > epp-values.yaml
inferenceExtension:
  pluginsCustomConfig:
    custom-plugins.yaml: |
      apiVersion: inference.networking.x-k8s.io/v1alpha1
      kind: EndpointPickerConfig
      plugins:
      - type: queue-scorer
      - type: kv-cache-utilization-scorer
      - type: round-robin-fairness-policy
        name: common-round-robin
      - type: concurrency-detector
        name: concurrency-detector
        parameters:
          maxConcurrency: 15
      schedulingProfiles:
      - name: default
        plugins:
        - pluginRef: queue-scorer
          weight: 2
        - pluginRef: kv-cache-utilization-scorer
          weight: 2
      featureGates:
      - flowControl
      flowControl:
        saturationDetectorRef: concurrency-detector
        defaultRequestTTL: 60s
        defaultPriorityBand:
          fairnessPolicyRef: common-round-robin
        priorityBands:
        - priority: 1
          fairnessPolicyRef: common-round-robin
        - priority: 0
          fairnessPolicyRef: common-round-robin
        - priority: -1
          fairnessPolicyRef: common-round-robin
EOF


step "6. Building custom EPP image"
IMAGE_TAG=docker.io/library/epp:local KIND_CLUSTER=inference-demo make image-kind


step "7. Deploying InferencePool and EPP via Helm"
export GATEWAY_PROVIDER=none
helm upgrade -i ${INFERENCE_POOL_NAME} \
  --set inferencePool.modelServers.matchLabels.app=${INFERENCE_POOL_NAME} \
  --set provider.name=$GATEWAY_PROVIDER \
  --set inferencePool.modelServerType=${MODEL_SERVER} \
  --set experimentalHttpRoute.enabled=true \
  --set targetModel=${MODEL_NAME} \
  --set inferenceExtension.pluginsConfigFile="custom-plugins.yaml" \
  --set inferenceExtension.image.hub="docker.io/library" \
  --set inferenceExtension.image.name="epp" \
  --set inferenceExtension.image.tag="local" \
  --set inferenceExtension.image.pullPolicy="Never" \
  -f epp-values.yaml \
  --create-namespace \
  --namespace default \
  config/charts/inferencepool

kubectl apply -f config/manifests/inferenceobjective.yaml
kubectl create clusterrolebinding epp-auth-delegator --clusterrole=system:auth-delegator --serviceaccount=default:vllm-qwen3-32b-epp --dry-run=client -o yaml | kubectl apply -f -


step "8. Installing Observability Stack (Prometheus & Grafana)"
kubectl create ns monitoring --dry-run=client -o yaml | kubectl apply -f -

kubectl apply -f config/observability/prometheus/rbac.yaml
kubectl create clusterrolebinding inference-gateway-sa-metrics-reader-role-binding \
  --clusterrole=inference-gateway-metrics-reader \
  --serviceaccount=monitoring:default --dry-run=client -o yaml | kubectl apply -f -

# Suppress noisy output from helm repo updates.
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts > /dev/null 2>&1
helm repo add grafana https://grafana.github.io/helm-charts > /dev/null 2>&1
helm repo update > /dev/null 2>&1

# Install Prometheus
helm upgrade -i prometheus prometheus-community/prometheus \
  --namespace monitoring \
  --create-namespace \
  -f config/observability/prometheus/values.yaml

# Provision the custom Grafana Dashboard
kubectl create configmap inference-gateway-dashboard \
  --namespace monitoring \
  --from-file=inference_gateway.json=tools/dashboards/inference_gateway.json \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl label configmap inference-gateway-dashboard --namespace monitoring grafana_dashboard=1 --overwrite

# Install Grafana
helm upgrade -i grafana grafana/grafana --namespace monitoring --create-namespace \
  --set "sidecar.dashboards.enabled=true" \
  --set "sidecar.dashboards.label=grafana_dashboard" \
  --set-string "sidecar.dashboards.labelValue=1" \
  --set "datasources.datasources\.yaml.apiVersion=1" \
  --set "datasources.datasources\.yaml.datasources[0].name=Prometheus" \
  --set "datasources.datasources\.yaml.datasources[0].type=prometheus" \
  --set "datasources.datasources\.yaml.datasources[0].url=http://prometheus-server.monitoring.svc.cluster.local:80" \
  --set "datasources.datasources\.yaml.datasources[0].access=proxy" \
  --set "datasources.datasources\.yaml.datasources[0].isDefault=true"


step "9. Waiting for Pods to Boot (Finalizing Setup)"

# Prevent 'kubectl wait' from crashing if the controller hasn't created the pods yet
echo "Waiting for Simulator and EPP Pods to be scheduled..."
while ! kubectl get pods -n default -l app=vllm-qwen3-32b 2>/dev/null | grep -q "vllm-qwen3"; do sleep 2; done
while ! kubectl get pods -n default -l inferencepool=vllm-qwen3-32b-epp 2>/dev/null | grep -q "epp"; do sleep 2; done
while ! kubectl get pods -n monitoring -l app.kubernetes.io/name=grafana 2>/dev/null | grep -q "grafana"; do sleep 2; done

echo "Waiting for Simulator Replicas to become Ready..."
kubectl wait --for=condition=Ready pod -l app=vllm-qwen3-32b --timeout=120s
echo "Waiting for Endpoint Picker (EPP) to become Ready..."
kubectl wait --for=condition=Ready pod -l inferencepool=vllm-qwen3-32b-epp --timeout=120s
echo "Waiting for InferencePool validation..."
kubectl wait --for=condition=Accepted inferencepool ${INFERENCE_POOL_NAME} --timeout=120s || echo -e "${YELLOW}Warning: InferencePool not yet accepted${NC}"
echo "Waiting for Grafana to become Ready..."
kubectl wait --for=condition=Ready pod -l app.kubernetes.io/name=grafana -n monitoring --timeout=120s


# Get the dynamically generated Grafana password
GRAFANA_PW=$(kubectl get secret --namespace monitoring grafana -o jsonpath="{.data.admin-password}" | base64 --decode)

echo -e "\n${GREEN}===================================================================${NC}"
echo -e "${GREEN}                  🎉 SETUP COMPLETE! 🎉                          ${NC}"
echo -e "${GREEN}===================================================================${NC}\n"

echo -e "Your cluster is ready for the Flow Control demo! Open two new terminal tabs and run these port-forwards:\n"
echo -e "${YELLOW}  1. Expose the API Gateway (for your Python Load Gen Script):${NC}"
echo "     kubectl port-forward svc/inference-gateway 8080:80"
echo ""
echo -e "${YELLOW}  2. Expose Grafana (for your Observability Dashboard):${NC}"
echo "     kubectl port-forward svc/grafana -n monitoring 3000:80"
echo ""
echo -e "${BLUE}Grafana Login:${NC}"
echo "  URL:      http://localhost:3000"
echo "  Username: admin"
echo "  Password: ${GRAFANA_PW}"
echo -e "\n${GREEN}===================================================================${NC}\n"
