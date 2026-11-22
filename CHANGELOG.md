# Changelog

All notable changes to VoxKube are documented here. The project follows [Semantic Versioning](https://semver.org/).
While the API is `v1alpha1`, minor releases (`0.y.0`) may contain breaking changes; they are listed under
**Breaking**.

GitHub Releases also carry auto-generated notes from merged pull requests.

## [Unreleased]

## [0.3.0]

Production workload controls (#36), Hiera Secret sharding (#37) and large control-repo guidance (#38). No breaking
changes: every new field defaults to the previous behaviour. Verified end to end on v0.3.0-rc.1.

**Upgrade notes:**
- Apply the 0.3.0 CRDs before upgrading the operator (README → "Applying a new release"). Coming from 0.2.5 or
  earlier, follow README → "From 0.2.5 or earlier to 0.3.x" first.
- The operator's ClusterRole gains `policy/poddisruptionbudgets`; upgrading the chart applies it.
- The CA, compiler and OpenVox DB pods roll once (new startup and liveness probes, compiler anti-affinity).
- With two or more compilers a PodDisruptionBudget `<cluster>-server` (`maxUnavailable: 1`) is created; node
  drains then evict one compiler at a time. Set `server.podDisruptionBudget.enabled: false` to opt out.
- Hiera is unchanged: `hieraSecrets.shards` defaults to 1, which keeps `<cluster>-hiera-secrets`.

### Added
- `spec.ca.javaArgs`/`resources` and `spec.database.javaArgs`/`resources` (#36).
- `spec.database.softWriteFailure` (default `false`) for `puppetdb.conf` on the compilers (#36).
- Startup and liveness probes on the CA, compilers and OpenVox DB (5-minute startup budget, liveness restarts only
  after 2 minutes of failures) (#36).
- PodDisruptionBudgets (`maxUnavailable: 1`) for the compilers and OpenVox DB whenever they run at least two
  replicas; switch off with `podDisruptionBudget.enabled: false` (#36).
- `spec.placement`: `nodeSelector`, `tolerations`, `affinity` and `imagePullSecrets` for all VoxCluster pods.
  Compilers prefer different nodes by default (soft pod anti-affinity) (#36).
- `spec.hieraSecrets.shards` (default 1, up to 32) spreads the Hiera data over several Secrets, merged on the
  compilers through a projected volume, so installations with many certificates stay under the 1 MiB Secret
  limit. A resize writes the new set before the compilers roll and deletes the old one afterwards (#37).
- `spec.hieraSecrets.flatCertificateKeys` (default `true`): `false` drops the duplicate `certs::<name>::*` keys
  and keeps only `openvox::certs` (#37).
- `status.hieraSecrets` (shard names and fill level) and the `HieraSecretsCapacity` condition, `False` with a
  Warning event at 80% of the limit. A shard over 1 MiB now fails the reconcile before anything is written,
  with a message naming `spec.hieraSecrets.shards` (#37).
- README: upgrade guide from 0.2.5 or earlier, and why `mode: imageVolume` suits large control-repos (#38).

## [0.2.9]

Git branch selection and PSK autosign hardening (#46, #39). No breaking changes.

**Upgrade notes:** the CA pod rolls once (new autosign script) and the compilers roll once (new
`VOXKUBE_GIT_BRANCHES` variable in the g10k containers). Code revisions are unchanged when `branches` is empty.

### Security
- The PSK autosign script parsed `openssl req -text` output with `sed`, which depended on the OpenSSL version and on
  how the key was encoded. It now runs in Puppet's Ruby: the CSR is decoded as ASN.1, its signature is verified,
  and the key is compared in constant time; any error denies (#39).

### Added
- `autosignMode: psk` also accepts the key in the `pp_preshared_key` extension request (#39).
- VoxDiscovery warns (Ready message and a `HighCardinalityLabels` event) when per-target labels split machines
  into almost one EndpointSlice each (#39).
- `VoxCluster.spec.code.git.branches`: deploy only the listed branches. Pushes to other branches no longer roll the
  compilers or trigger a sidecar redeploy, a broken `Puppetfile` on a feature branch can't break the cluster's
  code sync, and agents can't pick unrelated environments. Empty keeps every branch (#46).

## [0.2.8]

Fixes the security, correctness and performance findings of the architecture review of 0.2.7 (#27, #28, #29, #30,
#34). Verified end to end on k3s with an external agent (72 checks).

**Upgrade notes:** the CA, compilers and OpenVox DB roll once (new per-component certificates; the CA itself is
unchanged). cert-manager certificates of VoxNodes whose name contains a dot are re-issued once under a new name.
The CRDs now reject incomplete settings (see Added), so check `kubectl apply --dry-run=server` on your manifests.

### Security
- The CA private key and the `pp_cli_auth` certificate (full CA admin rights) were mounted into every compiler and
  OpenVox DB pod via `<cluster>-ca-tls`. Compilers and OpenVox DB now get their own certificates without
  `pp_cli_auth` in `<cluster>-server-tls` and `<cluster>-db-tls` (CA certificate and CRL included, CA key never);
  only the CA pod mounts `<cluster>-ca-tls` (#27).
- OpenVox DB accepted any certificate the CA signed, so every agent could read all nodes' facts and catalogs and
  submit commands. It now has a `certificate-allowlist` (compilers and operator, plus the new
  `database.certificateAllowlist`) and rejects revoked certificates (`ssl-crl-path`) (#27).

### Changed
- Each of the CA, compilers and OpenVox DB has a certificate with its own SANs, so changing the compilers' external
  hostname no longer rolls the CA or OpenVox DB. Upgrading rolls the CA, compilers and OpenVox DB once (#27).

### Added
- `VoxCluster.spec.database.certificateAllowlist` (#27).
- CRD validation (CEL, Kubernetes 1.25+): incomplete `psk`, `external` PostgreSQL, `sshKey`/`httpsToken`,
  `externalHttp` and VoxDiscovery `secretRef` settings are rejected at admission; `pollInterval`/`syncInterval` must
  be Go durations; VoxNode `certname` must be a valid lowercase certname; `certname` and `clusterRef` are
  immutable (#34).

### Changed (naming, RBAC, cache)
- **Breaking for FQDN-named VoxNodes:** generated cert-manager Certificate/Secret names keep the whole VoxNode name
  (dots → `-`) plus a hash, so `web01.us.example.com` and `web01.eu.example.com` no longer collide on
  `voxnode-cert-web01-…`. Certificates of VoxNodes whose name contains a dot are re-issued once under the new name
  (the old Certificate is deleted); Hiera keys don't change. Names without dots are unchanged. VoxDiscovery
  endpoint hostnames keep the whole certname too (#30).
- The ClusterRole no longer grants `batch/jobs`, `serviceaccounts` or access to cert-manager Issuers (#28).
- Secrets are no longer cached: the operator watches their metadata only and reads them from the API server,
  Hiera Secrets are listed with a server-side label selector, and unrelated Secrets (Helm releases, tokens) no
  longer trigger a VoxCluster reconcile (#28).
- `enc.sh` refuses empty certnames and ones containing `/`, `..` or spaces.
- Switching `code.mode` away from `git` deletes the g10k ConfigMap and the GitHub App token Secret; switching
  `authType` away from `githubApp` deletes the token Secret.

### Fixed (duplicates)
- Two VoxNodes with the same certname silently overwrote each other's ENC and Hiera data, and deleting either one
  revoked the certificate of the machine both described. The oldest now owns the certname; others report
  `DuplicateCertname`, are ignored, and are deleted without touching the certificate (#34).
- An agent could be signed for the cluster's own service certname, which OpenVox DB's allowlist trusts. Service
  names (`*.svc`) are now reserved and never signed via VoxNode or PSK autosigning (#34).

### Fixed
- VoxDiscovery reconciled in a loop (about once a second, querying OpenVox DB each time) because writing its own
  `lastSyncTime` re-triggered it; it now runs every `syncInterval` and on spec, label or annotation changes (#29).
- A VoxNode check-in (new `lastSeenTime`) re-ran the whole VoxCluster reconcile (Git poll, CRL fetch, every
  workload) and every VoxDiscovery; VoxCluster status changes (`readyReplicas`, `observedCommit`) re-ran every
  VoxNode. Controllers now only react to changes that matter to them (#29).
- VoxNode status, the HPA, VoxDiscovery's headless Service and EndpointSlices are written only when they change (#29).
- The ENC ConfigMap and the Hiera Secret had two writers (VoxCluster and every VoxNode); the VoxCluster controller is
  now the only one. It renders them first, so node changes reach the compilers even while a later step fails (#29).
- VoxDiscovery status writes outside the main sync path (invalid labels, missing cluster, database disabled, query
  failure) didn't retry on conflicts, so a stale read logged a 409.
- The mTLS HTTP client to the CA and OpenVox DB was rebuilt on every reconcile; it is now kept per VoxCluster and
  rebuilt only when the certificates change (#29).

## [0.2.7]

Fixes every finding of a full code review of 0.2.6.

### Breaking
- `voxkube_node_*` metrics no longer copy every `monitoring.voxkube.io/<label>` VoxNode label. Only the labels in
  the new `--node-metric-labels` flag (chart: `metrics.nodeLabels`, default `alerts`) are exported, so adding a
  label to one VoxNode no longer changes the series of all others. Add any other label you alert or route on.
- `externalHttp` node classification: an ENC server error, timeout or connection failure now fails the agent's
  catalog request instead of silently classifying the node with the default. HTTP 404 still falls back to the
  default.
- `VoxDiscovery.spec.serviceMonitor.targetLabels` keys must also be valid Kubernetes label keys (start with a
  letter, end with a letter or digit, at most 63 characters). They always ended up as Service labels, so other
  keys failed there anyway.
- The OpenVox DB Service no longer exposes plain HTTP port 8080; the listener is bound to localhost and used only
  by the readiness probe. Use the mTLS port 8081.

### Added
- `voxkube.io/skip-cleanup: "true"` on a VoxNode lets it be deleted without CA/OpenVox DB cleanup (for a CA that
  is lost for good).
- `--node-metric-labels` operator flag and `metrics.nodeLabels` chart value.

### Fixed
- OpenVox DB served unauthenticated HTTP on its Service; the plain listener is now localhost-only.
- GitHub App token minting failures unmounted the token Secret and made the operator poll unauthenticated;
  the last cached token now stays mounted, `CodeReady` reports `GitAuthFailed` and the compilers keep their
  revision.
- `code.git.pollInterval` below 10s is raised to 10s.
- `voxkube.io/sync-now` now also rolls the compilers in synchronized mode.
- SSH Git remotes failed under OpenShift's random UIDs (OpenSSH needs a passwd entry); `deploy.sh` now adds one
  and the openvox-server image makes `/etc/passwd` group-writable.
- A deleted VoxNode released its finalizer even when the CA clean or OpenVox DB deactivation failed; it now stays
  (`CleanupPending`) until both succeed, unless its VoxCluster is gone or being deleted.
- `Decommissioned` reported the cleanup as done even when it failed, repeated it on every reconcile and kept the
  last check-in, report status and certificate data.
- Nodes deactivated or expired in OpenVox DB were reported as `Active`; they now show `NodeDeactivated`.
- `Revoked` and deleting VoxNodes stayed in the ENC and Hiera data.
- The ENC was rendered before the VoxNode status was saved, so a role change took one more reconcile to apply.
- `PendingApproval` VoxNodes weren't requeued, so a CSR's `pp_role` showed up only after another change.
- Certificates removed from `VoxNode.spec.certificates` were never deleted; an existing foreign `Certificate`
  with the same name was taken over. The `secretName` field doc wrongly said it could point at an existing Secret.
- `autoCreateVoxNode` skipped certnames that had a VoxNode in another VoxCluster of the same namespace.
- Routes without a `host` got a new generated host on every reconcile, and annotations added by others were
  dropped; Routes, Certificates and ServiceMonitors are now updated only when their rendered content changes.
- Switching off OpenVox DB, the built-in PostgreSQL, the CA or the Hiera Secret left their objects running.
- The CA started with a PVC and a CA Secret from different backups; it now refuses to start with a clear error.
- Switching `serviceMonitor.enabled` off left the ServiceMonitor and the scraper certificate behind.
- `trusted.*` fact paths in VoxDiscovery selectors and `endpointLabels` found nothing, because the inventory
  query didn't return trusted data.
- VoxNodes with an empty `spec.state` (the API default) counted as inactive in the metrics.
- Labels and annotations the operator set on Routes, Certificates and ServiceMonitors are removed when dropped
  from the spec; labels, annotations, finalizers and owner references added by others are kept.
- Components switched off are pruned only after the compilers have rolled out without them.
- Nodes deactivated in OpenVox DB don't carry the old machine's report status (certname reuse).
- README examples used `role::webserver` as the role; roles used with role-scoped Hiera secrets must be valid
  label values, so the examples now use `webserver`.

## [0.2.6]

### Added
- Puppet run metrics per VoxNode on the operator's metrics endpoint: `voxkube_node_active`,
  `voxkube_node_last_report_failed` and `voxkube_node_last_seen_timestamp_seconds`, labelled with the node's
  certname, role and its `monitoring.voxkube.io/*` labels.
- README examples for a ServiceMonitor and the alerts `PuppetRunFailed` / `PuppetNodeNotReporting`. The operator
  only publishes metrics; scraping and alert rules stay in your own configuration.
- CI runs the chart's helm-unittest suite.

### Removed
- Chart: the unused `metrics.serviceMonitor` values (they rendered nothing).

### Fixed
- A compiler could run without any Puppet code. If GitHub was unreachable when a pod started, g10k-init deployed
  nothing but reported success, and the failed clone left a broken mirror in g10k's cache, so every later sync
  also deployed nothing. `deploy.sh` now refuses to deploy when `git ls-remote` fails or the remote has no
  branches, and treats a deployment without environments as a failure: it clears the g10k cache so the next
  attempt clones fresh, and doesn't record the state. g10k-init fails (the kubelet retries it), so a pod only
  starts once it has code; the sidecar retries on its next poll and keeps the existing code while GitHub is down.

## [0.2.5]

### Added
- `VoxNode.status.certificateExtensions`: the Puppet extensions (`pp_role`, `pp_datacenter`, …) of the node's
  pending CSR and, once signed, of its certificate. `pp_preshared_key` is never shown. While a node waits for
  approval, its message names the requested `pp_role`.
- `VoxNode.status.role` and the `ROLE` column: the effective role. When `spec.classification.role` is empty, the
  certificate's `pp_role` is passed to Puppet as `$role`, so role-scoped Hiera secrets (`voxkube.io/hiera-role`)
  also work for machines classified by `csr_attributes.yaml`. `classification.role` still overrides it.

### Fixed
- A signed node waiting for its first Puppet run was re-checked only every 5 minutes; it is now checked every 30 s.
- OpenVox DB query errors were logged at debug level only and the node looked like it was still waiting for its
  first run. They are now logged and shown in the `Ready` condition (`DatabaseUnreachable`), and an OpenVox DB
  outage no longer demotes an `Active` node to `Signed`.

### Changed
- The docs site is built from the latest release instead of `main`, so it always matches the released version.

## [0.2.4]

### Fixed
- `synchronizedRollout: false` (and SSH remotes): the `g10k-sync` sidecar ran a full g10k deployment and
  `puppet generate types` every `pollInterval`, even when nothing had changed. It now checks the branch heads
  with `git ls-remote` and redeploys only when one moved, plus once an hour for Puppetfile modules that track
  a branch. "g10k deployment finished" is logged only for real deployments.

### Added
- Docs site on GitHub Pages: overview, the README, and an interactive API explorer generated from the CRDs
  (search, permalinks to every field, types, defaults and validation).

## [0.2.3]

### Fixed
- Per-target labels (`endpointLabels`, VoxNode `monitoring.voxkube.io/<label>`) never reached Prometheus: they
  were written to the endpoints' `deprecatedTopology`, which the API server silently drops in
  `discovery.k8s.io/v1`. Machines are now grouped into EndpointSlices by label set, each slice labelled
  `label.voxkube.io/<label>`, and the ServiceMonitor maps `__meta_kubernetes_endpointslice_label_*` back.

### Changed
- Releases no longer rebuild the OpenVox images when `images/<image>/` is unchanged since the previous release;
  the existing image gets the new tags instead.

## [0.2.2]

### Added
- `VoxDiscovery.spec.serviceMonitor.targetLabels`: static labels (e.g. `role`, `alerts`) on every scraped target.
  They are set on the headless Service and copied to the targets by the ServiceMonitor's `targetLabels`.
- `VoxDiscovery.spec.endpointLabels`: per-target labels read from each node's facts in OpenVox DB, e.g.
  `role: trusted.extensions.pp_role`.
- VoxNode labels `monitoring.voxkube.io/<label>` become labels of that machine's targets and override
  `endpointLabels`, e.g. `monitoring.voxkube.io/alerts=false` to mute a machine. VoxNode changes are applied
  immediately.
- Invalid label names or values set `Ready=False` with reason `InvalidLabels`.

### Fixed
- `VoxDiscovery` ServiceMonitor found no targets: the operator publishes targets only as EndpointSlices, but
  Prometheus Operator discovers through Endpoints by default. The ServiceMonitor now sets
  `serviceDiscoveryRole: EndpointSlice`, and the `instance` relabeling reads
  `__meta_kubernetes_endpointslice_endpoint_node_name`. Prometheus needs read access to `endpointslices`.
- ServiceMonitor update errors are no longer ignored.

## [0.2.1]

### Fixed
- `code.mode: git`: `g10k-init` crashed with "invalid cross-device link" as soon as the `Puppetfile` had Forge
  modules. g10k hardlinks modules from its cache into the environments, and the cache was on a separate
  volume. The cache now lives on the code volume (`/etc/puppetlabs/code/.g10k-cache`).

### Changed
- Images are no longer built on every push to `main`: only releases and pre-releases push images, and pull
  requests build them without pushing. Unreleased code is tested through pre-releases (`v0.3.0-rc.1`).
- The weekly rebuild now refreshes the OpenVox images of the latest release and tags them `<openvox>-<YYYYMMDD>`,
  instead of rebuilding `main` and overwriting `<openvox>-<sha>`.

### Added
- Weekly GHCR cleanup workflow: keeps `latest`, every release/version tag and the newest 20 `<sha>`-only tags per
  image; deletes older ones and untagged leftovers (never manifests still referenced by a kept image).

## [0.2.0]

### Breaking
- `spec.networking` is split into `server` and `ca` blocks with the same fields. Move `externalHostname`,
  `additionalDNSNames`, `serviceType` and `openShiftRoute` under `networking.server`:

  ```yaml
  # 0.1.x                          # 0.2.0
  networking:                      networking:
    externalHostname: puppet.x       server:
    openShiftRoute:                    externalHostname: puppet.x
      enabled: true                    openShiftRoute:
                                         enabled: true
  ```
  Helm values move the same way (`cluster.networking.server.*`). Apply the new CRDs before upgrading the chart.

### Added
- The CA can be exposed to agents outside the cluster: `networking.ca` (external hostname, Service type,
  passthrough Route `<cluster>-ca`). Its hostnames are added to the service certificate.
- `status.serverExternalURL` and `status.caExternalURL` show what agents should use as `server` / `ca_server`.

### Fixed
- Disabling `openShiftRoute` now deletes the Route instead of leaving it behind.

## [0.1.0]: first release

### Added
- `VoxCluster`, `VoxNode` and `VoxDiscovery` (`voxkube.io/v1alpha1`), with the full API reference in the README.
- PKI: the operator generates the CA once and keeps it on VoxCluster deletion; the service certificate is
  renewed automatically; the CRL is synced from the CA to the compilers.
- CA autosign modes `voxnode`, `off`, `true` and `psk`; VoxNode auto-creation from CA state.
- OpenVox DB with external PostgreSQL (CloudNativePG) or ephemeral built-in PostgreSQL (dev/demo).
- Compilers send facts, catalogs and reports to OpenVox DB; ENC modes `voxnode`, `externalHttp` and `none`;
  report webhook; HorizontalPodAutoscaler.
- Code delivery via g10k (GitHub App, HTTPS token or SSH key; synchronized rollout or sidecar), PVC,
  image volume, or a demo environment.
- Honest status phases and conditions on all resources.
- Pinned OpenVox Server and OpenVoxDB **8.18.0** images, with Renovate-driven updates.
- Versioned operator builds (`--version`, startup log), release pipeline (images, OCI Helm chart, GitHub Release),
  usable from the GitHub UI or by pushing a tag.
- `VoxDiscovery` stands down (`Ready=False`, `DatabaseDisabled`) when the `VoxCluster` has no database.

[Unreleased]: https://github.com/yboikov/voxkube-operator/compare/v0.2.6...HEAD
[0.2.6]: https://github.com/yboikov/voxkube-operator/compare/v0.2.5...v0.2.6
[0.2.5]: https://github.com/yboikov/voxkube-operator/compare/v0.2.4...v0.2.5
[0.2.4]: https://github.com/yboikov/voxkube-operator/compare/v0.2.3...v0.2.4
[0.2.3]: https://github.com/yboikov/voxkube-operator/compare/v0.2.2...v0.2.3
[0.2.2]: https://github.com/yboikov/voxkube-operator/compare/v0.2.1...v0.2.2
[0.2.1]: https://github.com/yboikov/voxkube-operator/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/yboikov/voxkube-operator/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/yboikov/voxkube-operator/releases/tag/v0.1.0
