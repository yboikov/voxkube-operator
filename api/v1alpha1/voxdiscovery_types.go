package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// DiscoverySelectorSpec filters active PuppetDB nodes for EndpointSlice export.
type DiscoverySelectorSpec struct {
	// Environment filters nodes by Puppet environment (e.g., "production").
	// +optional
	Environment string `json:"environment,omitempty"`

	// Facts filters nodes by exact fact key/value matches (e.g., {"kernel": "Linux"}).
	// +optional
	Facts map[string]string `json:"facts,omitempty"`

	// Classes filters nodes whose last catalog contains all of these classes (e.g. "profile::node_exporter").
	// +optional
	Classes []string `json:"classes,omitempty"`

	// PQL is an additional Puppet Query Language filter expression ANDed into the inventory query,
	// e.g. `facts.os.family = "RedHat"`. Write only the expression that goes inside inventory[...] { }.
	// +optional
	PQL string `json:"pql,omitempty"`
}

// DiscoveryPort defines a port exposed on the discovered EndpointSlices and headless Service.
type DiscoveryPort struct {
	// Name of the port (e.g., "metrics", "blackbox").
	Name string `json:"name"`

	// Port number (e.g., 9100 for node_exporter).
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`

	// Protocol is the network protocol (TCP, UDP, SCTP).
	// +kubebuilder:default="TCP"
	Protocol corev1.Protocol `json:"protocol,omitempty"`
}

// DiscoveryTLSSpec configures mTLS for Prometheus ServiceMonitor scraping of external VM exporters.
// +kubebuilder:validation:XValidation:rule="self.mode != 'secretRef' || (has(self.secretName) && self.secretName != '')",message="tls mode secretRef requires secretName"
type DiscoveryTLSSpec struct {
	// Enabled turns on TLS/mTLS on the generated ServiceMonitor.
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`

	// Mode selects "openvoxCA" (zero-touch mTLS using the OpenVox CA and auto-minted prometheus-scraper client cert)
	// or "secretRef" (custom TLS Secret).
	// +kubebuilder:validation:Enum=openvoxCA;secretRef
	// +kubebuilder:default="openvoxCA"
	Mode string `json:"mode,omitempty"`

	// SecretName is the TLS Secret name when mode is "secretRef".
	// +optional
	SecretName string `json:"secretName,omitempty"`
}

// DiscoveryServiceMonitorSpec configures automatic Prometheus ServiceMonitor creation.
type DiscoveryServiceMonitorSpec struct {
	// Enabled creates a monitoring.coreos.com/v1 ServiceMonitor targeting the headless Service.
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`

	// Interval is the Prometheus scrape interval.
	// +kubebuilder:default="30s"
	Interval string `json:"interval,omitempty"`

	// ScrapeTimeout is the Prometheus scrape timeout.
	// +kubebuilder:default="10s"
	ScrapeTimeout string `json:"scrapeTimeout,omitempty"`

	// PreserveNodeHostnameAsInstance adds relabelConfigs so the VM's Puppet certname becomes the Prometheus instance label.
	// +kubebuilder:default=true
	PreserveNodeHostnameAsInstance *bool `json:"preserveNodeHostnameAsInstance,omitempty"`

	// AdditionalLabels are extra labels added to the ServiceMonitor metadata.
	// +optional
	AdditionalLabels map[string]string `json:"additionalLabels,omitempty"`

	// TargetLabels are static labels added to every scraped target, e.g. {"role": "web", "alerts": "true"}.
	// They are set on the headless Service and copied to the targets through the ServiceMonitor's targetLabels.
	// Keys must be Prometheus label names ([a-zA-Z_][a-zA-Z0-9_]*, not starting with "__") other than job,
	// instance, namespace, service, endpoint, pod and container. Values must be valid Kubernetes label values.
	// +optional
	TargetLabels map[string]string `json:"targetLabels,omitempty"`

	// TLS configures HTTPS / mTLS scraping of VM exporters.
	// +optional
	TLS DiscoveryTLSSpec `json:"tls,omitempty"`
}

// VoxDiscoverySpec defines the desired state of a VoxDiscovery.
type VoxDiscoverySpec struct {
	// ClusterRef is the name of the VoxCluster whose OpenVox DB (PuppetDB) is queried.
	ClusterRef string `json:"clusterRef"`

	// SyncInterval controls how frequently the operator queries PuppetDB and updates EndpointSlices.
	// +kubebuilder:default="60s"
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(ns|us|ms|s|m|h))+$`
	SyncInterval string `json:"syncInterval,omitempty"`

	// Selector filters which active nodes in PuppetDB are exported.
	// +optional
	Selector DiscoverySelectorSpec `json:"selector,omitempty"`

	// AddressFact is the Puppet fact path used to extract each node's IP address.
	// +kubebuilder:default="networking.ip"
	AddressFact string `json:"addressFact,omitempty"`

	// Ports lists the network ports exposed in the headless Service and EndpointSlices.
	Ports []DiscoveryPort `json:"ports"`

	// EndpointLabels adds per-target labels read from each node's OpenVox DB facts: target label name → fact
	// path, e.g. {"role": "trusted.extensions.pp_role", "os": "os.family"}. Values that are not valid Kubernetes
	// label values (e.g. containing spaces, longer than 63 characters) are skipped. Labels on the node's VoxNode
	// named monitoring.voxkube.io/<label> are added too and override EndpointLabels.
	// Names must match [a-zA-Z]([a-zA-Z0-9_]*[a-zA-Z0-9])? and must not be reserved Prometheus labels or also
	// appear in serviceMonitor.targetLabels.
	// +optional
	EndpointLabels map[string]string `json:"endpointLabels,omitempty"`

	// ServiceMonitor optionally creates a Prometheus ServiceMonitor with hostname relabeling and mTLS.
	// +optional
	ServiceMonitor DiscoveryServiceMonitorSpec `json:"serviceMonitor,omitempty"`
}

// VoxDiscoveryStatus defines the observed state of a VoxDiscovery.
type VoxDiscoveryStatus struct {
	// DiscoveredNodes is the number of active VMs currently exported into EndpointSlices.
	// +optional
	DiscoveredNodes int32 `json:"discoveredNodes,omitempty"`

	// EndpointSliceCount is the number of EndpointSlice objects managed for this discovery.
	// +optional
	EndpointSliceCount int32 `json:"endpointSliceCount,omitempty"`

	// ServiceName is the name of the headless Service created for this discovery.
	// +optional
	ServiceName string `json:"serviceName,omitempty"`

	// ScraperSecretName is the name of the mTLS client certificate Secret minted when tls.mode is "openvoxCA".
	// +optional
	ScraperSecretName string `json:"scraperSecretName,omitempty"`

	// LastSyncTime is the timestamp of the last successful PuppetDB sync.
	// +optional
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`

	// Conditions represent the latest available observations of the VoxDiscovery state.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=vd
// +kubebuilder:printcolumn:name="Cluster",type="string",JSONPath=".spec.clusterRef"
// +kubebuilder:printcolumn:name="Nodes",type="integer",JSONPath=".status.discoveredNodes"
// +kubebuilder:printcolumn:name="Slices",type="integer",JSONPath=".status.endpointSliceCount"
// +kubebuilder:printcolumn:name="Service",type="string",JSONPath=".status.serviceName"
// +kubebuilder:printcolumn:name="Last Sync",type="date",JSONPath=".status.lastSyncTime"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// VoxDiscovery is the Schema for the voxdiscoveries API.
type VoxDiscovery struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VoxDiscoverySpec   `json:"spec,omitempty"`
	Status VoxDiscoveryStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// VoxDiscoveryList contains a list of VoxDiscovery.
type VoxDiscoveryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VoxDiscovery `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &VoxDiscovery{}, &VoxDiscoveryList{})
		return nil
	})
}
