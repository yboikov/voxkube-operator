# VoxKube End-to-End Test Suite (`hack/e2e`)

Repeatable 14-stage E2E test suite that validates a released (or pre-released) `voxkube-operator` on a single `x86_64` Ubuntu host running k3s, an external Docker `openvox-agent` EL9 container, and a self-contained in-cluster Git HTTP+SSH server.

## Requirements

- Ubuntu 24.04+ `x86_64` host (`amd64`), root access
- Recommended: 4 vCPU, 8 GB RAM, 20+ GB disk (works on 2 vCPU / 4 GB RAM with `replicas: 1`)
- Outbound HTTPS to `ghcr.io`, `quay.io`, `registry.access.redhat.com`, `yum.voxpupuli.org`, and `github.com`

## Usage

Copy `hack/e2e/` to your test host (or clone the repo on it) and run:

```bash
# Test a full release:
VOXKUBE_VERSION=0.3.0 ./hack/e2e/run-all.sh

# Test a pre-release that also changed images/openvox-server/ (override the server image tag):
VOXKUBE_VERSION=0.3.1-rc.1 \
  SERVER_IMAGE=ghcr.io/yboikov/openvox-server:8.18.0-5d2c2cf \
  ./hack/e2e/run-all.sh
```

Or run stages individually:

1. `00-bootstrap-vps.sh` — idempotently installs swap, Docker, Helm, lean k3s (with Ubuntu 26.04 containerd AppArmor workaround), and builds/imports `voxkube-e2e-agent:el9` and `voxkube-e2e-git:latest`.
2. `01-install-operator.sh` — creates `openvox` with `pod-security.kubernetes.io/enforce=restricted`, server-side applies release CRDs, and installs/upgrades `voxkube-operator` via Helm with `metrics.nodeLabels={alerts,team}`.
3. `02-run-e2e.sh` — resets any previous run and executes 14 stages:
   - `VoxCluster` bring-up under PSS `restricted` + arbitrary OpenShift-like UID `1000720000:0` + NodePort `server`/`ca`
   - OpenVox DB `:8080` localhost-only hardening (Service exposes only mTLS `:8081`), PKI split verification (no `ca.key` or `pp_cli_auth` cert in any compiler or DB pod; DB mounts `crl.pem` + `certificate-allowlist`), and production workload controls (`startupProbe`/`livenessProbe`, DB/CA `javaArgs` & `resources`, `softWriteFailure`, compiler PDB, soft pod anti-affinity, `HieraSecretsCapacity`)
   - External Docker `openvox-agent` CSR with `pp_role`/`pp_datacenter` → `autoCreateVoxNode` (`PendingApproval`)
   - CSR signing, `pp_role` ENC fallback, role-scoped Hiera `Sensitive` secrets, `puppet agent -t`, PuppetDB `Signed → Active` (facts + reports stored), and `certificate-allowlist` rejecting direct agent mTLS queries (`HTTP 403`)
   - Git/g10k sync (`httpsToken`, `synchronizedRollout: true`, real `Puppetfile` with `puppetlabs-stdlib` from Forge, `environment.conf` + environment `hiera.yaml`, `role::webserver` → `profile::node_exporter` starting real Prometheus `node_exporter`, `spec.code.git.branches` branch filtering, `spec.hieraSecrets.shards` `1 → 3` atomic resize + projected-volume Hiera lookup + old-shard pruning, commit push rollout, `voxkube.io/sync-now` rollout)
   - SSH Git remote (`authType: sshKey`) under random UID `1000720000:0` (`GitSidecar`)
   - `VoxDiscovery` (`selector.classes: [profile::node_exporter]`, `EndpointSlice`s, live HTTP scrape of the discovered `node_exporter` target, `openvoxCA` scraper TLS cert, `trusted.extensions.pp_role`, `monitoring.voxkube.io/*`, `InvalidLabels`)
   - Operator `/metrics` (`voxkube_node_*` + `--node-metric-labels` allowlist, plus all five certificate expiry metric families verified against real CA/scraper/agent X.509 `NotAfter` timestamps and `VoxNode.status.certificateNotAfter`)
   - `voxkube.io/paused: "true"` cascade (`VoxCluster` → `VoxNode` / `VoxDiscovery` `ClusterPaused`), frozen spec edits, paused deletion finalizer hold + `voxkube.io/skip-cleanup` escape hatch, and resume on unpause
   - `VoxNode` revocation (ENC/Hiera removal), finalizer hold (`CleanupPending`) while CA is down, clean CA+DB decommission without resurrection
   - Ruby PSK autosign (`autosignMode: psk` + `pp_preshared_key`: wrong key rejected, valid key autosigned)
   - Disabled component pruning (`database.enabled=false`, `hieraSecrets.enabled=false`), DB/scraper expiry metric cleanup, and CA retention (`<cluster>-ca-tls` and `<cluster>-ca-data`)

## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `VOXKUBE_VERSION` | `Chart.yaml` version (or `0.3.0`) | Operator chart & CRD release version (without leading `v`) |
| `SERVER_IMAGE` | *(chart/operator default)* | Optional override for `VoxCluster.spec.server.image` |
| `NAMESPACE` | `openvox` | Namespace for the operator and test `VoxCluster` |
| `CLUSTER` | `e2e` | Test `VoxCluster` name |
| `VPS_IP` | auto-detected (`hostname -I`) | Host IP for `--add-host` on the external Docker agent |
| `E2E_LOG` | `/tmp/voxkube-e2e-last-run.log` | Combined log file written by `run-all.sh` |
