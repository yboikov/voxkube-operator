# Security Policy

VoxKube runs a Puppet certificate authority and handles its private key, agent certificates and Hiera
secrets, so please report security problems privately.

## Reporting a vulnerability

**Do not open a public issue.** Use GitHub's private reporting instead:
[Report a vulnerability](https://github.com/yboikov/voxkube-operator/security/advisories/new)
(repository **Security** tab → **Report a vulnerability**).

Please include the VoxKube version (`kubectl -n <namespace> logs deploy/voxkube-operator | head -1`), what an
attacker can do, and steps to reproduce. You should get a first answer within a week. This is a hobby
project maintained by one person, so fixes are best effort.

## Supported versions

Only the latest release receives fixes. While the API is `v1alpha1`, upgrade to the newest `0.y.z` to get a fix.

## Scope

In scope: the operator, the Helm chart, the container images built here (`ghcr.io/yboikov/voxkube-operator`,
`openvox-server`, `openvox-db`) and the CI workflows.

Out of scope: vulnerabilities in OpenVox / Puppet themselves (report them to
[OpenVox](https://github.com/OpenVoxProject)), and in Kubernetes, OpenShift or other third-party components.
