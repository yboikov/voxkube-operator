package controller

// OpenVox versions this operator release is built and tested with. They are used when
// VoxCluster.spec.server.image / spec.database.image are empty.
//
// Keep these in sync with:
//   - images/openvox-server/Containerfile (ARG OPENVOX_SERVER_VERSION, ARG OPENVOXDB_VERSION)
//   - images/openvox-db/Containerfile (ARG OPENVOXDB_VERSION)
//   - charts/voxkube-operator/values.yaml and templates/voxcluster.yaml
//
// Renovate bumps all of them in one PR (see renovate.json).
const (
	// renovate: datasource=github-releases depName=OpenVoxProject/openvox-server
	DefaultOpenVoxServerVersion = "8.18.0"
	// renovate: datasource=github-releases depName=OpenVoxProject/openvoxdb
	DefaultOpenVoxDBVersion = "8.18.0"

	DefaultOpenVoxServerImage = "ghcr.io/yboikov/openvox-server:" + DefaultOpenVoxServerVersion
	DefaultOpenVoxDBImage     = "ghcr.io/yboikov/openvox-db:" + DefaultOpenVoxDBVersion
)
