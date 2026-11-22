#!/usr/bin/env bash
# Creates the local kind cluster for k8s-sentinel and installs Calico as the
# CNI, since kind's default CNI (kindnet) does not enforce NetworkPolicy.
# See README.md "Key design decisions" for why Calico is used here even
# though the tool itself only ever writes standard NetworkPolicy objects.
set -euo pipefail

CLUSTER_NAME="k8s-sentinel"
CALICO_VERSION="v3.33.0"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

echo "==> Checking prerequisites..."
for cmd in docker kind kubectl; do
  if ! command -v "$cmd" >/dev/null 2>&1; then
    echo "ERROR: '$cmd' is not installed or not on PATH. See README.md Prerequisites." >&2
    exit 1
  fi
done
if ! docker version >/dev/null 2>&1; then
  echo "ERROR: Docker daemon is not reachable. Start Docker Desktop and try again." >&2
  exit 1
fi

echo "==> Creating kind cluster '${CLUSTER_NAME}' (CNI disabled)..."
kind create cluster --name "${CLUSTER_NAME}" --config "${SCRIPT_DIR}/kind-cluster-config.yaml"

echo "==> Determining Kubernetes server version to pick the right Calico CRD manifest..."
K8S_MINOR=$(kubectl version -o json | python3 -c "import sys,json; v=json.load(sys.stdin)['serverVersion']; print(int(v['minor'].rstrip('+')))")
if [ "${K8S_MINOR}" -ge 36 ]; then
  CRD_MANIFEST="v3_projectcalico_org.yaml"
else
  CRD_MANIFEST="v3_projectcalico_org-v1beta1.yaml"
fi
echo "    Server minor version: 1.${K8S_MINOR} -> using ${CRD_MANIFEST}"

echo "==> Installing Calico CRDs (${CALICO_VERSION})..."
kubectl create -f "https://raw.githubusercontent.com/projectcalico/calico/${CALICO_VERSION}/manifests/${CRD_MANIFEST}"

echo "==> Installing the Tigera Operator..."
kubectl create -f "https://raw.githubusercontent.com/projectcalico/calico/${CALICO_VERSION}/manifests/tigera-operator.yaml"

echo "==> Installing Calico via its custom resources..."
kubectl create -f "https://raw.githubusercontent.com/projectcalico/calico/${CALICO_VERSION}/manifests/custom-resources.yaml"

echo "==> Waiting for Calico components to become available (this can take a few minutes)..."
for i in $(seq 1 60); do
  STATUS_LINES=$(kubectl get tigerastatus --no-headers 2>/dev/null)
  # Treat "no resources yet" (empty output, operator still reconciling) as NOT ready,
  # not as success — the previous version of this loop broke immediately on empty
  # output because `grep -qv` returns non-zero on zero input lines, not just on an
  # all-True result. Only break once every row's AVAILABLE column reads exactly True.
  if [ -n "${STATUS_LINES}" ] && ! echo "${STATUS_LINES}" | awk '{print $2}' | grep -qv '^True$'; then
    break
  fi
  sleep 5
done
kubectl get tigerastatus

echo "==> Waiting for all nodes to become Ready..."
kubectl wait --for=condition=Ready nodes --all --timeout=180s

echo "==> Cluster is ready. Context: kind-${CLUSTER_NAME}"
kubectl get nodes -o wide
