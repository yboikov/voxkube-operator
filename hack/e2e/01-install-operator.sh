#!/usr/bin/env bash
# Installs or upgrades voxkube-operator (and its CRDs) into the target namespace
# with Kubernetes Pod Security Admission "restricted" enforced.
set -euo pipefail

export KUBECONFIG="${KUBECONFIG:-/etc/rancher/k3s/k3s.yaml}"

if [ -z "${VOXKUBE_VERSION:-}" ]; then
  CHART_YAML="$(cd "$(dirname "$0")/../.." 2>/dev/null && pwd)/charts/voxkube-operator/Chart.yaml"
  if [ -f "${CHART_YAML}" ]; then
    VOXKUBE_VERSION="$(awk '/^version:/ {print $2; exit}' "${CHART_YAML}")"
  else
    VOXKUBE_VERSION="0.3.0"
  fi
fi

NAMESPACE="${NAMESPACE:-openvox}"

echo "=== [01] Installing VoxKube Operator ${VOXKUBE_VERSION} into namespace ${NAMESPACE} ==="

kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
# Enforce Kubernetes PSS "restricted" (closest vanilla K8s equivalent to OpenShift restricted-v2)
kubectl label namespace "${NAMESPACE}" \
  pod-security.kubernetes.io/enforce=restricted \
  pod-security.kubernetes.io/audit=restricted \
  pod-security.kubernetes.io/warn=restricted \
  --overwrite

# Apply release CRDs via server-side apply (as documented in README -> Upgrading)
CRD_URL="https://github.com/yboikov/voxkube-operator/releases/download/v${VOXKUBE_VERSION}/voxkube-crds.yaml"
echo "Applying CRDs from ${CRD_URL}..."
curl -fsSL "${CRD_URL}" | kubectl apply --server-side --force-conflicts -f -

helm upgrade --install voxkube-operator oci://ghcr.io/yboikov/charts/voxkube-operator \
  --version "${VOXKUBE_VERSION}" \
  --namespace "${NAMESPACE}" \
  --set "metrics.nodeLabels={alerts,team}" \
  --wait --timeout 180s

kubectl -n "${NAMESPACE}" rollout status deployment/voxkube-operator --timeout=120s
kubectl -n "${NAMESPACE}" logs deployment/voxkube-operator --tail=10
echo "=== [01] Operator ready ==="
