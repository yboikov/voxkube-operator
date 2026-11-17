# Changelog

All notable changes to VoxKube are documented here. The project follows [Semantic Versioning](https://semver.org/).
While the API is `v1alpha1`, minor releases (`0.y.0`) may contain breaking changes; they are listed under
**Breaking**.

GitHub Releases also carry auto-generated notes from merged pull requests.

## [Unreleased]

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

[Unreleased]: https://github.com/yboikov/voxkube-operator/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/yboikov/voxkube-operator/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/yboikov/voxkube-operator/releases/tag/v0.1.0
