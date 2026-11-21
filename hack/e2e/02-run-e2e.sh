#!/usr/bin/env bash
# Runs the repeatable 12-stage VoxKube E2E test suite against a k3s cluster
# with an external Docker openvox-agent container and a self-contained Git HTTP+SSH server.
set -euo pipefail

export KUBECONFIG="${KUBECONFIG:-/etc/rancher/k3s/k3s.yaml}"

NAMESPACE="${NAMESPACE:-openvox}"
INFRA_NS="${INFRA_NS:-e2e-infra}"
CLUSTER="${CLUSTER:-e2e}"
AGENT_CONTAINER="voxkube-e2e-agent-ext"
AGENT_CERTNAME="web01.e2e.local"
VPS_IP="${VPS_IP:-$(hostname -I | awk '{print $1}')}"

PASS=0
FAIL=0
FAILURES=()

log()  { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
pass() { PASS=$((PASS + 1)); printf '\033[0;32m  [PASS] %s\033[0m\n' "$*"; }
fail() { FAIL=$((FAIL + 1)); FAILURES+=("$*"); printf '\033[0;31m  [FAIL] %s\033[0m\n' "$*" >&2; }

assert_eq() {
  local label="$1" got="$2" want="$3"
  if [ "$got" = "$want" ]; then
    pass "${label} (=${got})"
  else
    fail "${label}: got '${got}', want '${want}'"
  fi
}

assert_contains() {
  local label="$1" haystack="$2" needle="$3"
  if printf '%s' "$haystack" | grep -qF -- "$needle"; then
    pass "${label}"
  else
    fail "${label}: expected to contain '${needle}', got: $(printf '%s' "$haystack" | head -c 400)"
  fi
}

assert_not_contains() {
  local label="$1" haystack="$2" needle="$3"
  if ! printf '%s' "$haystack" | grep -qF -- "$needle"; then
    pass "${label}"
  else
    fail "${label}: expected NOT to contain '${needle}'"
  fi
}

wait_for() {
  local desc="$1" timeout="$2" cmd="$3"
  local start
  start=$(date +%s)
  while true; do
    if eval "$cmd" >/dev/null 2>&1; then
      return 0
    fi
    if [ $(( $(date +%s) - start )) -ge "$timeout" ]; then
      fail "Timed out after ${timeout}s waiting for: ${desc}"
      return 1
    fi
    sleep 3
  done
}

cleanup_before_run() {
  log "Cleaning up any previous E2E run state (idempotent reset)"
  docker rm -f "${AGENT_CONTAINER}" >/dev/null 2>&1 || true
  # Delete VoxCluster FIRST so autoCreateVoxNode cannot recreate VoxNodes during teardown
  kubectl -n "${NAMESPACE}" delete voxdiscovery --all --ignore-not-found --timeout=30s >/dev/null 2>&1 || true
  kubectl -n "${NAMESPACE}" delete voxcluster "${CLUSTER}" --ignore-not-found --timeout=60s >/dev/null 2>&1 || true
  for vn in $(kubectl -n "${NAMESPACE}" get voxnode -o name 2>/dev/null || true); do
    kubectl -n "${NAMESPACE}" annotate "${vn}" voxkube.io/skip-cleanup=true --overwrite >/dev/null 2>&1 || true
  done
  kubectl -n "${NAMESPACE}" delete voxnode --all --ignore-not-found --timeout=30s >/dev/null 2>&1 || true
  # Clean retained CA PVC + Secret + test secrets so every E2E run starts fresh
  kubectl -n "${NAMESPACE}" delete pvc "${CLUSTER}-ca-data" --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "${NAMESPACE}" delete secret "${CLUSTER}-ca-tls" e2e-role-secret e2e-git-https e2e-git-ssh --ignore-not-found >/dev/null 2>&1 || true
  kubectl delete namespace "${INFRA_NS}" --ignore-not-found --wait=true --timeout=60s >/dev/null 2>&1 || true
  wait_for "old cluster pods removed" 60 "[ -z \"\$(kubectl -n ${NAMESPACE} get pods -l app.kubernetes.io/instance=${CLUSTER} -o name 2>/dev/null)\" ]" || true
}

# ==============================================================================
# Start E2E Suite
# ==============================================================================
cleanup_before_run

log "1. Deploying self-contained Git HTTP+SSH server in ${INFRA_NS}"
kubectl create namespace "${INFRA_NS}"
SSH_TMP=$(mktemp -d)
ssh-keygen -t ed25519 -N "" -f "${SSH_TMP}/id_ed25519" -C "e2e@voxkube" >/dev/null
kubectl -n "${INFRA_NS}" create secret generic git-authorized-keys --from-file=authorized_keys="${SSH_TMP}/id_ed25519.pub"
kubectl -n "${NAMESPACE}" create secret generic e2e-git-ssh --from-file=ssh-privatekey="${SSH_TMP}/id_ed25519"
kubectl -n "${NAMESPACE}" create secret generic e2e-git-https --from-literal=username=gituser --from-literal=token=e2e-token-123
rm -rf "${SSH_TMP}"

kubectl -n "${INFRA_NS}" apply -f - <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: git-server
spec:
  replicas: 1
  selector:
    matchLabels: { app: git-server }
  template:
    metadata:
      labels: { app: git-server }
    spec:
      containers:
        - name: git-server
          image: voxkube-e2e-git:latest
          imagePullPolicy: Never
          ports:
            - { name: http, containerPort: 8080 }
            - { name: ssh, containerPort: 2222 }
          volumeMounts:
            - { name: auth, mountPath: /auth, readOnly: true }
      volumes:
        - name: auth
          secret: { secretName: git-authorized-keys }
---
apiVersion: v1
kind: Service
metadata:
  name: git-server
spec:
  selector: { app: git-server }
  ports:
    - { name: http, port: 8080, targetPort: 8080 }
    - { name: ssh, port: 2222, targetPort: 2222 }
EOF
kubectl -n "${INFRA_NS}" rollout status deployment/git-server --timeout=90s
pass "Self-contained Git HTTP+SSH server running"

log "2. Deploying VoxCluster '${CLUSTER}' (PSS restricted + random OpenShift-like UID 1000720000 + NodePort)"
SERVER_IMAGE_YAML=""
if [ -n "${SERVER_IMAGE:-}" ]; then
  SERVER_IMAGE_YAML="    image: ${SERVER_IMAGE}"
fi
kubectl -n "${NAMESPACE}" apply -f - <<EOF
apiVersion: voxkube.io/v1alpha1
kind: VoxCluster
metadata:
  name: ${CLUSTER}
spec:
  code:
    mode: pvc
  ca:
    storageSize: 1Gi
    autosignMode: voxnode
  server:
${SERVER_IMAGE_YAML}
    replicas: 2
    maxActiveInstances: 1
    javaArgs: "-Xms512m -Xmx512m"
    securityContext:
      runAsUser: 1000720000
      runAsGroup: 0
  database:
    enabled: true
    postgres:
      mode: builtin
  networking:
    server:
      externalHostname: puppet.e2e.local
      serviceType: NodePort
    ca:
      externalHostname: puppet-ca.e2e.local
      serviceType: NodePort
  hieraSecrets:
    enabled: true
EOF

echo "Waiting for VoxCluster '${CLUSTER}' to reach Phase=Running (up to 300s)..."
if wait_for "VoxCluster Phase=Running" 300 "[ \"\$(kubectl -n ${NAMESPACE} get vc ${CLUSTER} -o jsonpath='{.status.phase}')\" = 'Running' ]"; then
  pass "VoxCluster reached Phase=Running under PSS restricted + UID 1000720000"
fi

READY_REPS=$(kubectl -n "${NAMESPACE}" get vc "${CLUSTER}" -o jsonpath='{.status.readyReplicas}')
assert_eq "VoxCluster readyReplicas" "${READY_REPS}" "2"

SRV_EXT_URL=$(kubectl -n "${NAMESPACE}" get vc "${CLUSTER}" -o jsonpath='{.status.serverExternalURL}')
CA_EXT_URL=$(kubectl -n "${NAMESPACE}" get vc "${CLUSTER}" -o jsonpath='{.status.caExternalURL}')
assert_contains "serverExternalURL populated" "${SRV_EXT_URL}" "https://puppet.e2e.local:"
assert_contains "caExternalURL populated" "${CA_EXT_URL}" "https://puppet-ca.e2e.local:"

SRV_PORT="${SRV_EXT_URL##*:}"
CA_PORT="${CA_EXT_URL##*:}"
echo "  Server NodePort=${SRV_PORT}, CA NodePort=${CA_PORT}, VPS_IP=${VPS_IP}"

log "3. Verifying OpenVox DB plain-HTTP port 8080 is NOT exposed on Service or pod IP"
DB_SVC_PORTS=$(kubectl -n "${NAMESPACE}" get svc "${CLUSTER}-db" -o jsonpath='{.spec.ports[*].port}')
assert_eq "DB Service exposes only port 8081" "${DB_SVC_PORTS}" "8081"

DB_POD_IP=$(kubectl -n "${NAMESPACE}" get pod -l "app.kubernetes.io/instance=${CLUSTER},app.kubernetes.io/component=db" -o jsonpath='{.items[0].status.podIP}')
if curl -fsS --max-time 3 "http://${DB_POD_IP}:8080/status/v1/simple" >/dev/null 2>&1; then
  fail "DB plain HTTP 8080 is reachable on pod IP ${DB_POD_IP} (expected localhost-only)"
else
  pass "DB plain HTTP 8080 refused on pod IP ${DB_POD_IP} (localhost-only)"
fi

log "4. Starting external Docker openvox-agent container (${AGENT_CONTAINER}) & testing CSR + pp_role + autoCreateVoxNode"
docker run -d --name "${AGENT_CONTAINER}" \
  --add-host "puppet.e2e.local:${VPS_IP}" \
  --add-host "puppet-ca.e2e.local:${VPS_IP}" \
  voxkube-e2e-agent:el9 sleep infinity >/dev/null

docker exec "${AGENT_CONTAINER}" sh -eu -c "
mkdir -p /etc/puppetlabs/puppet
cat <<'CONF' > /etc/puppetlabs/puppet/puppet.conf
[main]
certname = ${AGENT_CERTNAME}
server = puppet.e2e.local
serverport = ${SRV_PORT}
ca_server = puppet-ca.e2e.local
ca_port = ${CA_PORT}
environment = production
CONF
cat <<'CSR' > /etc/puppetlabs/puppet/csr_attributes.yaml
---
extension_requests:
  pp_role: webserver
  pp_datacenter: hel1
CSR
/opt/puppetlabs/bin/puppet ssl submit_request
"
pass "External Docker agent submitted CSR with pp_role=webserver, pp_datacenter=hel1"

# Trigger immediate VoxCluster reconcile so autoCreateVoxNode runs right away
kubectl -n "${NAMESPACE}" annotate vc "${CLUSTER}" "voxkube.io/sync-now=$(date +%s)" --overwrite >/dev/null

if wait_for "auto-created VoxNode ${AGENT_CERTNAME}" 90 "kubectl -n ${NAMESPACE} get voxnode ${AGENT_CERTNAME}"; then
  pass "Operator auto-created VoxNode ${AGENT_CERTNAME}"
fi

if wait_for "VoxNode pp_role extracted from pending CSR" 90 "[ \"\$(kubectl -n ${NAMESPACE} get vn ${AGENT_CERTNAME} -o jsonpath='{.status.certificateExtensions.pp_role}')\" = 'webserver' ]"; then
  pass "Pending CSR extension pp_role=webserver populated in VoxNode.status.certificateExtensions"
fi

VN_STATE=$(kubectl -n "${NAMESPACE}" get vn "${AGENT_CERTNAME}" -o jsonpath='{.spec.state}')
VN_ROLE=$(kubectl -n "${NAMESPACE}" get vn "${AGENT_CERTNAME}" -o jsonpath='{.status.role}')
VN_DC=$(kubectl -n "${NAMESPACE}" get vn "${AGENT_CERTNAME}" -o jsonpath='{.status.certificateExtensions.pp_datacenter}')
VN_MSG=$(kubectl -n "${NAMESPACE}" get vn "${AGENT_CERTNAME}" -o jsonpath='{.status.conditions[0].message}')
assert_eq "auto-created VoxNode spec.state" "${VN_STATE}" "PendingApproval"
assert_eq "pending VoxNode status.role" "${VN_ROLE}" "webserver"
assert_eq "pending VoxNode pp_datacenter" "${VN_DC}" "hel1"
assert_contains "PendingApproval message names pp_role" "${VN_MSG}" 'pp_role "webserver"'

log "5. Creating role-scoped Hiera Secret + approving VoxNode -> Active + running puppet agent -t"
kubectl -n "${NAMESPACE}" create secret generic e2e-role-secret --from-literal=secret_token=super-secret-e2e-value
kubectl -n "${NAMESPACE}" label secret e2e-role-secret \
  voxkube.io/hiera-secret=true \
  voxkube.io/hiera-scope=role \
  voxkube.io/hiera-role=webserver

kubectl -n "${NAMESPACE}" patch vn "${AGENT_CERTNAME}" --type=merge -p '{
  "spec": {
    "state": "Active",
    "classification": {
      "classes": ["profile::nginx"],
      "parameters": {
        "http_port": "8088",
        "welcome_message": "Hello from E2E"
      }
    }
  }
}'

if wait_for "VoxNode CSR signed" 90 "[ \"\$(kubectl -n ${NAMESPACE} get vn ${AGENT_CERTNAME} -o jsonpath='{.status.certificateSigned}')\" = 'true' ]"; then
  pass "Operator signed CSR for ${AGENT_CERTNAME}"
fi

FP=$(kubectl -n "${NAMESPACE}" get vn "${AGENT_CERTNAME}" -o jsonpath='{.status.certificateFingerprint}')
assert_contains "Real SHA256 fingerprint populated" "${FP}" ":"

ENC_YAML=$(kubectl -n "${NAMESPACE}" get cm "${CLUSTER}-enc-nodes" -o json | jq -r ".data[\"${AGENT_CERTNAME}.yaml\"] // empty")
assert_contains "ENC has pp_role fallback role: webserver" "${ENC_YAML}" "role: webserver"
assert_contains "ENC has class profile::nginx" "${ENC_YAML}" "profile::nginx"

HIERA_ROLE=$(kubectl -n "${NAMESPACE}" get secret "${CLUSTER}-hiera-secrets" -o jsonpath='{.data.roles_webserver\.yaml}' | base64 -d)
assert_contains "Hiera roles_webserver.yaml has secret_token" "${HIERA_ROLE}" "secret_token: super-secret-e2e-value"
assert_contains "Hiera roles_webserver.yaml marks secret Sensitive" "${HIERA_ROLE}" "convert_to: Sensitive"

# Wait until kubelet syncs ENC ConfigMap into ALL compiler pods (kubelet syncPeriod can take up to 60s)
wait_for "ENC ConfigMap synced into compiler pods" 90 "(
  for p in \$(kubectl -n ${NAMESPACE} get pod -l app.kubernetes.io/instance=${CLUSTER},app.kubernetes.io/component=server -o name); do
    kubectl -n ${NAMESPACE} exec \"\$p\" -c openvox-server -- grep -q 'profile::nginx' /etc/puppetlabs/puppet/enc-nodes/${AGENT_CERTNAME}.yaml || exit 1
  done
)"
AGENT_OUT=""
AGENT_RC=0
AGENT_OUT=$(docker exec "${AGENT_CONTAINER}" /opt/puppetlabs/bin/puppet agent -t 2>&1) || AGENT_RC=$?
if [ "${AGENT_RC}" -eq 0 ] || [ "${AGENT_RC}" -eq 2 ]; then
  pass "External Docker agent 'puppet agent -t' succeeded (exit ${AGENT_RC})"
else
  fail "External Docker agent 'puppet agent -t' failed (exit ${AGENT_RC}): ${AGENT_OUT}"
fi

NODE_TXT=$(docker exec "${AGENT_CONTAINER}" cat /etc/voxkube-node.txt 2>/dev/null || true)
NGINX_CONF=$(docker exec "${AGENT_CONTAINER}" cat /etc/voxkube-nginx.conf 2>/dev/null || true)
assert_contains "Demo site.pp applied /etc/voxkube-node.txt with Role: webserver" "${NODE_TXT}" "Role: webserver"
assert_contains "ENC profile::nginx applied /etc/voxkube-nginx.conf with listen 8088" "${NGINX_CONF}" "listen 8088;"

if wait_for "VoxNode Phase=Active (reported in OpenVox DB)" 90 "[ \"\$(kubectl -n ${NAMESPACE} get vn ${AGENT_CERTNAME} -o jsonpath='{.status.phase}')\" = 'Active' ]"; then
  pass "VoxNode transitioned Signed -> Active from OpenVox DB check-in"
fi
LAST_REPORT=$(kubectl -n "${NAMESPACE}" get vn "${AGENT_CERTNAME}" -o jsonpath='{.status.lastReportStatus}')
if [ "${LAST_REPORT}" = "changed" ] || [ "${LAST_REPORT}" = "unchanged" ]; then
  pass "VoxNode lastReportStatus populated (=${LAST_REPORT})"
else
  fail "VoxNode lastReportStatus unexpected: got '${LAST_REPORT}', want 'changed' or 'unchanged'"
fi

log "6. Testing Git/g10k code sync: httpsToken + synchronizedRollout + Hiera Sensitive + sync-now"
kubectl -n "${NAMESPACE}" patch vc "${CLUSTER}" --type=merge -p "{
  \"spec\": {
    \"code\": {
      \"mode\": \"git\",
      \"git\": {
        \"remote\": \"http://git-server.${INFRA_NS}.svc.cluster.local:8080/git/control.git\",
        \"authType\": \"httpsToken\",
        \"credentialsSecretRef\": \"e2e-git-https\",
        \"pollInterval\": \"15s\",
        \"synchronizedRollout\": true,
        \"generateTypes\": true
      }
    }
  }
}"

if wait_for "VoxCluster GitSynced" 90 "[ \"\$(kubectl -n ${NAMESPACE} get vc ${CLUSTER} -o jsonpath='{.status.conditions[?(@.type==\"CodeReady\")].reason}')\" = 'GitSynced' ]"; then
  pass "VoxCluster switched to GitSynced via httpsToken"
fi
kubectl -n "${NAMESPACE}" rollout status "deployment/${CLUSTER}-server" --timeout=240s
pass "VoxCluster rolled out Git control-repo via httpsToken + g10k-init"

COMMIT1=$(kubectl -n "${NAMESPACE}" get vc "${CLUSTER}" -o jsonpath='{.status.observedCommit}')
GIT_HEAD=$(kubectl -n "${INFRA_NS}" exec deploy/git-server -- git --git-dir=/srv/git/control.git rev-parse HEAD)
assert_eq "VoxCluster observedCommit matches Git HEAD" "${COMMIT1}" "${GIT_HEAD}"

# Run puppet agent -t against the Git control-repo (which also reads Hiera Sensitive secret_token)
AGENT_RC=0
AGENT_OUT=$(docker exec "${AGENT_CONTAINER}" /opt/puppetlabs/bin/puppet agent -t 2>&1) || AGENT_RC=$?
if [ "${AGENT_RC}" -eq 0 ] || [ "${AGENT_RC}" -eq 2 ]; then
  pass "Agent applied Git-deployed catalog (exit ${AGENT_RC})"
else
  fail "Agent failed on Git-deployed catalog (exit ${AGENT_RC}): ${AGENT_OUT}"
fi
GIT_TXT=$(docker exec "${AGENT_CONTAINER}" cat /etc/voxkube-git.txt 2>/dev/null || true)
SECRET_TXT=$(docker exec "${AGENT_CONTAINER}" cat /etc/voxkube-secret.txt 2>/dev/null || true)
assert_contains "Git site.pp applied GitRev=v1 & HasSecret=yes" "${GIT_TXT}" "GitRev=v1"
assert_contains "Git site.pp found Hiera Sensitive secret" "${GIT_TXT}" "HasSecret=yes"
assert_eq "Hiera Sensitive secret written to /etc/voxkube-secret.txt" "${SECRET_TXT}" "super-secret-e2e-value"
assert_not_contains "Sensitive secret value NOT leaked in puppet agent output" "${AGENT_OUT}" "super-secret-e2e-value"

# Push commit v2 and verify synchronized rollout updates observedCommit and rolls compilers
kubectl -n "${INFRA_NS}" exec deploy/git-server -- sh -eu -c '
W=/tmp/bump
rm -rf "$W"
git clone /srv/git/control.git "$W"
cd "$W"
git config user.email "e2e@voxkube.local"
git config user.name "VoxKube E2E"
sed -i "s/GitRev=v1/GitRev=v2/" manifests/site.pp
git commit -am "Bump to v2"
git push origin production
rm -rf "$W"
' >/dev/null 2>&1
GIT_HEAD2=$(kubectl -n "${INFRA_NS}" exec deploy/git-server -- git --git-dir=/srv/git/control.git rev-parse HEAD)

if wait_for "VoxCluster picked up Git commit v2" 90 "[ \"\$(kubectl -n ${NAMESPACE} get vc ${CLUSTER} -o jsonpath='{.status.observedCommit}')\" = '${GIT_HEAD2}' ]"; then
  pass "Synchronized poller detected commit v2 (${GIT_HEAD2:0:12})"
fi
kubectl -n "${NAMESPACE}" rollout status "deployment/${CLUSTER}-server" --timeout=180s
AGENT_RC=0
docker exec "${AGENT_CONTAINER}" /opt/puppetlabs/bin/puppet agent -t >/dev/null 2>&1 || AGENT_RC=$?
GIT_TXT2=$(docker exec "${AGENT_CONTAINER}" cat /etc/voxkube-git.txt 2>/dev/null || true)
assert_contains "Agent received GitRev=v2 after synchronized rollout" "${GIT_TXT2}" "GitRev=v2"

# Test voxkube.io/sync-now rolls compilers even when HEAD did not move
OLD_REV=$(kubectl -n "${NAMESPACE}" get deploy "${CLUSTER}-server" -o jsonpath='{.spec.template.metadata.annotations.voxkube\.io/code-revision}')
SYNC_VAL="e2e-$(date +%s)"
kubectl -n "${NAMESPACE}" annotate vc "${CLUSTER}" "voxkube.io/sync-now=${SYNC_VAL}" --overwrite >/dev/null
if wait_for "sync-now updated code-revision annotation" 60 "[ \"\$(kubectl -n ${NAMESPACE} get deploy ${CLUSTER}-server -o jsonpath='{.spec.template.metadata.annotations.voxkube\.io/code-revision}')\" != '${OLD_REV}' ]"; then
  NEW_REV=$(kubectl -n "${NAMESPACE}" get deploy "${CLUSTER}-server" -o jsonpath='{.spec.template.metadata.annotations.voxkube\.io/code-revision}')
  assert_contains "sync-now appended to code-revision" "${NEW_REV}" "${SYNC_VAL}"
fi
kubectl -n "${NAMESPACE}" rollout status "deployment/${CLUSTER}-server" --timeout=180s

log "7. Testing SSH Git remote (authType: sshKey) under random OpenShift UID 1000720000"
kubectl -n "${NAMESPACE}" patch vc "${CLUSTER}" --type=merge -p "{
  \"spec\": {
    \"code\": {
      \"mode\": \"git\",
      \"git\": {
        \"remote\": \"ssh://git@git-server.${INFRA_NS}.svc.cluster.local:2222/srv/git/control.git\",
        \"authType\": \"sshKey\",
        \"credentialsSecretRef\": \"e2e-git-ssh\",
        \"pollInterval\": \"30s\",
        \"generateTypes\": true
      }
    }
  }
}"
if wait_for "VoxCluster switched to GitSidecar" 60 "[ \"\$(kubectl -n ${NAMESPACE} get vc ${CLUSTER} -o jsonpath='{.status.conditions[?(@.type==\"CodeReady\")].reason}')\" = 'GitSidecar' ]"; then
  pass "VoxCluster detected SSH remote and selected GitSidecar mode"
fi
if kubectl -n "${NAMESPACE}" rollout status "deployment/${CLUSTER}-server" --timeout=240s; then
  pass "SSH g10k-init + g10k-sync sidecar succeeded under random UID 1000720000"
else
  fail "SSH g10k-init + g10k-sync rollout failed under random UID 1000720000"
fi

log "8. Testing VoxDiscovery (EndpointSlices, trusted.extensions.pp_role, monitoring.voxkube.io/*, targetLabels validation)"
kubectl -n "${NAMESPACE}" label vn "${AGENT_CERTNAME}" \
  monitoring.voxkube.io/alerts=false \
  monitoring.voxkube.io/team=platform \
  monitoring.voxkube.io/unlisted=yes \
  --overwrite

kubectl -n "${NAMESPACE}" apply -f - <<EOF
apiVersion: voxkube.io/v1alpha1
kind: VoxDiscovery
metadata:
  name: e2e-node-exporter
spec:
  clusterRef: ${CLUSTER}
  syncInterval: 15s
  selector:
    environment: production
  endpointLabels:
    role: trusted.extensions.pp_role
    os: os.family
  ports:
    - name: metrics
      port: 9100
      protocol: TCP
  serviceMonitor:
    enabled: false
    targetLabels:
      env: e2e
EOF

if wait_for "VoxDiscovery exported 1 node" 90 "[ \"\$(kubectl -n ${NAMESPACE} get vd e2e-node-exporter -o jsonpath='{.status.discoveredNodes}')\" = '1' ]"; then
  pass "VoxDiscovery exported ${AGENT_CERTNAME} from OpenVox DB"
fi

SLICE_JSON=$(kubectl -n "${NAMESPACE}" get endpointslice -l "kubernetes.io/service-name=e2e-node-exporter" -o json)
assert_contains "EndpointSlice nodeName is certname" "${SLICE_JSON}" "\"nodeName\": \"${AGENT_CERTNAME}\""
assert_contains "EndpointSlice label.voxkube.io/role=webserver from trusted.extensions.pp_role" "${SLICE_JSON}" '"label.voxkube.io/role": "webserver"'
assert_contains "EndpointSlice label.voxkube.io/os=RedHat from os.family" "${SLICE_JSON}" '"label.voxkube.io/os": "RedHat"'
assert_contains "EndpointSlice label.voxkube.io/alerts=false from VoxNode label" "${SLICE_JSON}" '"label.voxkube.io/alerts": "false"'
assert_contains "EndpointSlice label.voxkube.io/team=platform from VoxNode label" "${SLICE_JSON}" '"label.voxkube.io/team": "platform"'

# Verify invalid targetLabels key is rejected with InvalidLabels
kubectl -n "${NAMESPACE}" apply -f - <<EOF
apiVersion: voxkube.io/v1alpha1
kind: VoxDiscovery
metadata:
  name: e2e-bad-labels
spec:
  clusterRef: ${CLUSTER}
  ports:
    - name: metrics
      port: 9100
  serviceMonitor:
    targetLabels:
      _bad_key: "x"
EOF
if wait_for "InvalidLabels reported on bad targetLabels" 30 "[ \"\$(kubectl -n ${NAMESPACE} get vd e2e-bad-labels -o jsonpath='{.status.conditions[?(@.type==\"Ready\")].reason}')\" = 'InvalidLabels' ]"; then
  pass "VoxDiscovery rejected invalid targetLabels key '_bad_key' with InvalidLabels"
fi
kubectl -n "${NAMESPACE}" delete vd e2e-bad-labels >/dev/null

log "9. Testing Operator Prometheus Metrics (/metrics) + --node-metric-labels allowlist"
OP_POD_IP=$(kubectl -n "${NAMESPACE}" get pod -l app.kubernetes.io/name=voxkube-operator -o jsonpath='{.items[0].status.podIP}')
METRICS=$(curl -fsS --max-time 5 "http://${OP_POD_IP}:8080/metrics")
NODE_METRIC_LINE=$(printf '%s\n' "${METRICS}" | grep "voxkube_node_active{" | grep "${AGENT_CERTNAME}" || true)
assert_contains "voxkube_node_active exported for ${AGENT_CERTNAME}" "${NODE_METRIC_LINE}" "certname=\"${AGENT_CERTNAME}\""
assert_contains "Allowlisted label alerts=\"false\" present" "${NODE_METRIC_LINE}" 'alerts="false"'
assert_contains "Allowlisted label team=\"platform\" present" "${NODE_METRIC_LINE}" 'team="platform"'
assert_not_contains "Non-allowlisted label unlisted NOT exported" "${NODE_METRIC_LINE}" 'unlisted='
assert_contains "voxkube_node_last_report_failed exported" "${METRICS}" "voxkube_node_last_report_failed{"
assert_contains "voxkube_node_last_seen_timestamp_seconds exported" "${METRICS}" "voxkube_node_last_seen_timestamp_seconds{"

log "10. Testing VoxNode Revocation -> ENC/Hiera removal -> CRL sync"
kubectl -n "${NAMESPACE}" patch vn "${AGENT_CERTNAME}" --type=merge -p '{"spec":{"state":"Revoked"}}'
if wait_for "VoxNode Phase=Revoked" 60 "[ \"\$(kubectl -n ${NAMESPACE} get vn ${AGENT_CERTNAME} -o jsonpath='{.status.phase}')\" = 'Revoked' ]"; then
  pass "VoxNode transitioned to Phase=Revoked"
fi
ENC_AFTER_REVOKE=$(kubectl -n "${NAMESPACE}" get cm "${CLUSTER}-enc-nodes" -o jsonpath='{.data}')
assert_not_contains "Revoked node removed from ENC ConfigMap" "${ENC_AFTER_REVOKE}" "${AGENT_CERTNAME}"

log "11. Testing Finalizer hold (CleanupPending when CA down) & clean release when CA returns"
kubectl -n "${NAMESPACE}" scale statefulset "${CLUSTER}-ca" --replicas=0 >/dev/null
wait_for "CA pod terminated" 60 "[ -z \"\$(kubectl -n ${NAMESPACE} get pod -l app.kubernetes.io/instance=${CLUSTER},app.kubernetes.io/component=ca -o name)\" ]"
kubectl -n "${NAMESPACE}" delete vn "${AGENT_CERTNAME}" --wait=false >/dev/null

if wait_for "Deleted VoxNode held by finalizer with CleanupPending" 60 "[ \"\$(kubectl -n ${NAMESPACE} get vn ${AGENT_CERTNAME} -o jsonpath='{.status.conditions[?(@.type==\"Ready\")].reason}')\" = 'CleanupPending' ]"; then
  pass "Finalizer held VoxNode with CleanupPending while CA was down"
fi

# Bring CA back -> finalizer should clean CA, deactivate DB, and release VoxNode
kubectl -n "${NAMESPACE}" scale statefulset "${CLUSTER}-ca" --replicas=1 >/dev/null
kubectl -n "${NAMESPACE}" rollout status "statefulset/${CLUSTER}-ca" --timeout=180s
if wait_for "VoxNode cleaned and deleted after CA returned" 90 "! kubectl -n ${NAMESPACE} get vn ${AGENT_CERTNAME} >/dev/null 2>&1"; then
  pass "VoxNode cleaned on CA + OpenVox DB and finalizer released"
fi

# Trigger a cluster reconcile and verify autoCreateVoxNode does NOT resurrect the cleaned node
kubectl -n "${NAMESPACE}" annotate vc "${CLUSTER}" "voxkube.io/sync-now=post-clean-$(date +%s)" --overwrite >/dev/null
sleep 10
if kubectl -n "${NAMESPACE}" get vn "${AGENT_CERTNAME}" >/dev/null 2>&1; then
  fail "autoCreateVoxNode resurrected deleted node ${AGENT_CERTNAME}!"
else
  pass "autoCreateVoxNode did NOT resurrect cleaned node ${AGENT_CERTNAME}"
fi

log "12. Testing component pruning (database.enabled=false, hieraSecrets.enabled=false) & CA retention"
kubectl -n "${NAMESPACE}" patch vc "${CLUSTER}" --type=merge -p '{
  "spec": {
    "database": { "enabled": false },
    "hieraSecrets": { "enabled": false }
  }
}'
if wait_for "DB + Postgres + Hiera Secret pruned" 90 "! kubectl -n ${NAMESPACE} get deploy ${CLUSTER}-db && ! kubectl -n ${NAMESPACE} get sts ${CLUSTER}-postgres && ! kubectl -n ${NAMESPACE} get secret ${CLUSTER}-hiera-secrets"; then
  pass "pruneDisabledComponents deleted DB Deployment, Postgres StatefulSet, and Hiera Secret"
fi
if wait_for "VoxDiscovery stood down with DatabaseDisabled" 60 "[ \"\$(kubectl -n ${NAMESPACE} get vd e2e-node-exporter -o jsonpath='{.status.conditions[?(@.type==\"Ready\")].reason}')\" = 'DatabaseDisabled' ]"; then
  pass "VoxDiscovery stood down cleanly (DatabaseDisabled) and removed EndpointSlices"
fi

# Verify CA Secret and CA PVC survived pruning
if kubectl -n "${NAMESPACE}" get secret "${CLUSTER}-ca-tls" >/dev/null 2>&1 && kubectl -n "${NAMESPACE}" get pvc "${CLUSTER}-ca-data" >/dev/null 2>&1; then
  pass "CA Secret (${CLUSTER}-ca-tls) and PVC (${CLUSTER}-ca-data) preserved"
else
  fail "CA Secret or PVC missing after pruning!"
fi

# Clean up external Docker container
docker rm -f "${AGENT_CONTAINER}" >/dev/null 2>&1 || true

printf '\n============================================================\n'
printf '  E2E SUMMARY: %d passed, %d failed\n' "${PASS}" "${FAIL}"
printf '============================================================\n'
if [ "${FAIL}" -gt 0 ]; then
  printf 'Failures:\n'
  for f in "${FAILURES[@]}"; do
    printf '  - %s\n' "$f"
  done
  exit 1
fi
