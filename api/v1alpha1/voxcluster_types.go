package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// VoxClusterPhase describes the overall lifecycle phase of a VoxCluster.
// +kubebuilder:validation:Enum=Pending;InitializingCA;DeployingDatabase;DeployingServer;Running;Degraded;Error
type VoxClusterPhase string

const (
	ClusterPhasePending           VoxClusterPhase = "Pending"
	ClusterPhaseInitializingCA    VoxClusterPhase = "InitializingCA"
	ClusterPhaseDeployingDatabase VoxClusterPhase = "DeployingDatabase"
	ClusterPhaseDeployingServer   VoxClusterPhase = "DeployingServer"
	ClusterPhaseRunning           VoxClusterPhase = "Running"
	ClusterPhaseDegraded          VoxClusterPhase = "Degraded"
	ClusterPhaseError             VoxClusterPhase = "Error"
)

// ClusterCodeSpec configures how Puppet environments and modules are deployed into the cluster.
// When the selected mode has no source configured (mode "git" without git, "pvc" without claimName,
// "imageVolume" without image) a small demo "production" environment is seeded instead.
type ClusterCodeSpec struct {
	// Mode selects the code deployment strategy.
	// +kubebuilder:validation:Enum=git;pvc;imageVolume
	// +kubebuilder:default="git"
	Mode string `json:"mode,omitempty"`

	// Git configures outbound-only Git polling and g10k deployment of the control-repo.
	// +optional
	Git *GitCodeSpec `json:"git,omitempty"`

	// ClaimName is the PVC mounted read-only at /etc/puppetlabs/code when mode is "pvc".
	// It must contain an environments/ directory. Use a ReadWriteMany or ReadOnlyMany PVC when replicas > 1.
	// +optional
	ClaimName string `json:"claimName,omitempty"`

	// Image is an OCI image mounted read-only at /etc/puppetlabs/code when mode is "imageVolume"
	// (Kubernetes image volumes). It must contain an environments/ directory.
	// +optional
	Image string `json:"image,omitempty"`
}

// GitCodeSpec configures outbound-only Git synchronization via g10k.
// +kubebuilder:validation:XValidation:rule="(self.authType != 'sshKey' && self.authType != 'httpsToken') || (has(self.credentialsSecretRef) && self.credentialsSecretRef != '')",message="authType sshKey and httpsToken require credentialsSecretRef"
type GitCodeSpec struct {
	// Remote is the Git repository URL for the Puppet control-repo. Every branch becomes a Puppet environment.
	Remote string `json:"remote"`

	// AuthType selects the Git authentication mechanism.
	// +kubebuilder:validation:Enum=githubApp;sshKey;httpsToken;none
	// +kubebuilder:default="githubApp"
	AuthType string `json:"authType,omitempty"`

	// GitHubApp configures native GitHub App authentication (RS256 JWT -> auto-rotated installation token).
	// +optional
	GitHubApp *GitHubAppAuthSpec `json:"githubApp,omitempty"`

	// CredentialsSecretRef names a Secret in the same namespace.
	// For authType "sshKey" it must contain "ssh-privatekey" and may contain "known_hosts".
	// For authType "httpsToken" it must contain either "token" (optionally with "username",
	// default "x-access-token") or "username" and "password".
	// +optional
	CredentialsSecretRef string `json:"credentialsSecretRef,omitempty"`

	// PollInterval is how often the remote is checked for new commits (Go duration, e.g. "60s", "5m").
	// +kubebuilder:default="60s"
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(ns|us|ms|s|m|h))+$`
	PollInterval string `json:"pollInterval,omitempty"`

	// SynchronizedRollout makes code changes reach all compilers together: the operator polls the remote and
	// performs a rolling restart of the compilers when any branch changes, so every pod serves the same revision.
	// When false, every compiler runs a g10k sidecar that deploys independently every pollInterval.
	// SSH remotes cannot be polled by the operator and always use the sidecar.
	// +kubebuilder:default=true
	SynchronizedRollout *bool `json:"synchronizedRollout,omitempty"`

	// GenerateTypes runs "puppet generate types" for every environment after each g10k deployment.
	// +kubebuilder:default=true
	GenerateTypes *bool `json:"generateTypes,omitempty"`
}

// GitHubAppAuthSpec configures native GitHub App authentication for outbound Git polling and g10k.
type GitHubAppAuthSpec struct {
	// AppID is the GitHub App numeric ID.
	// +kubebuilder:validation:Minimum=1
	AppID int64 `json:"appID"`

	// InstallationID is the GitHub App installation numeric ID.
	// +kubebuilder:validation:Minimum=1
	InstallationID int64 `json:"installationID"`

	// PrivateKeySecretRef references the Kubernetes Secret holding the GitHub App RSA private key PEM.
	PrivateKeySecretRef corev1.SecretKeySelector `json:"privateKeySecretRef"`

	// APIBaseURL is the GitHub API base URL (defaults to https://api.github.com; override for GitHub Enterprise Server).
	// +kubebuilder:default="https://api.github.com"
	APIBaseURL string `json:"apiBaseURL,omitempty"`
}

// ClusterCASpec configures the OpenVox Certificate Authority.
// +kubebuilder:validation:XValidation:rule="self.autosignMode != 'psk' || (has(self.pskSecretRef) && self.pskSecretRef != '')",message="autosignMode psk requires pskSecretRef"
type ClusterCASpec struct {
	// Enabled controls whether the built-in OpenVox CA is managed.
	// +kubebuilder:default=true
	Enabled *bool `json:"enabled,omitempty"`

	// StorageSize is the PVC size for the CA data directory (/etc/puppetlabs/puppetserver/ca).
	// +kubebuilder:default="2Gi"
	StorageSize string `json:"storageSize,omitempty"`

	// StorageClassName is the optional StorageClass for the CA PVC.
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`

	// TTL is the root CA certificate validity, applied when the CA is first generated.
	// Accepts Go durations plus "d" (days) and "y" (365 days) suffixes, e.g. "10y", "3650d", "87600h".
	// +kubebuilder:default="10y"
	TTL string `json:"ttl,omitempty"`

	// AutosignMode controls how incoming CSRs are signed:
	// "voxnode" signs a CSR when a VoxNode for its certname is Active;
	// "off" never signs automatically (VoxNode only reports certificate state);
	// "true" makes the CA sign every CSR immediately;
	// "psk" makes the CA sign CSRs whose challengePassword attribute matches the pre-shared key.
	// +kubebuilder:validation:Enum=voxnode;psk;off;true
	// +kubebuilder:default="voxnode"
	AutosignMode string `json:"autosignMode,omitempty"`

	// PSKSecretRef names a Secret with key "psk" holding the pre-shared key when autosignMode is "psk".
	// +optional
	PSKSecretRef string `json:"pskSecretRef,omitempty"`
}

// ClusterServerSpec configures the OpenVox Server catalog compilers.
type ClusterServerSpec struct {
	// Image is the OpenVox Server container image. When empty, the operator uses the OpenVox Server version it was
	// released with (ghcr.io/yboikov/openvox-server:<version>, see internal/controller/versions.go).
	// +optional
	Image string `json:"image,omitempty"`

	// ImagePullPolicy is the container image pull policy.
	// +kubebuilder:validation:Enum=Always;IfNotPresent;Never
	// +kubebuilder:default="IfNotPresent"
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`

	// Replicas is the number of catalog compiler pods.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=2
	Replicas *int32 `json:"replicas,omitempty"`

	// JavaArgs configures JVM heap settings for OpenVox Server.
	// +kubebuilder:default="-Xms1g -Xmx1g"
	JavaArgs string `json:"javaArgs,omitempty"`

	// MaxActiveInstances is the JRuby pool size per compiler pod.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=2
	MaxActiveInstances int32 `json:"maxActiveInstances,omitempty"`

	// Resources specifies CPU and memory requests/limits for compiler pods.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// Autoscaling configures HorizontalPodAutoscaler for the compiler pool.
	// +optional
	Autoscaling *ServerAutoscalingSpec `json:"autoscaling,omitempty"`

	// SecurityContext allows optional override of the PodSecurityContext.
	// By default RunAsUser, RunAsGroup, and FSGroup are nil for 100% OpenShift restricted-v2 SCC compliance.
	// +optional
	SecurityContext *PodSecurityContextSpec `json:"securityContext,omitempty"`
}

// PodSecurityContextSpec overrides the pod-level security context defaults.
// By default all UID/GID fields are left nil so OpenShift restricted-v2 SCC injects the namespace range.
type PodSecurityContextSpec struct {
	// RunAsUser overrides the UID the pod runs as.
	// +optional
	RunAsUser *int64 `json:"runAsUser,omitempty"`

	// RunAsGroup overrides the primary GID the pod runs as.
	// +optional
	RunAsGroup *int64 `json:"runAsGroup,omitempty"`

	// RunAsNonRoot overrides whether the pod must run as a non-root user.
	// +optional
	RunAsNonRoot *bool `json:"runAsNonRoot,omitempty"`

	// FSGroup overrides the supplemental group applied to mounted volumes.
	// +optional
	FSGroup *int64 `json:"fsGroup,omitempty"`

	// FSGroupChangePolicy controls when volume ownership is changed to match FSGroup.
	// +kubebuilder:validation:Enum=Always;OnRootMismatch
	// +optional
	FSGroupChangePolicy *corev1.PodFSGroupChangePolicy `json:"fsGroupChangePolicy,omitempty"`
}

// ServerAutoscalingSpec configures HorizontalPodAutoscaler for compilers.
type ServerAutoscalingSpec struct {
	// Enabled turns on HPA for the compiler Deployment.
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`

	// MinReplicas is the minimum number of compiler replicas.
	// +kubebuilder:default=2
	MinReplicas *int32 `json:"minReplicas,omitempty"`

	// MaxReplicas is the maximum number of compiler replicas.
	// +kubebuilder:default=6
	MaxReplicas int32 `json:"maxReplicas,omitempty"`

	// TargetCPUUtilizationPercentage is the CPU target for HPA.
	// +kubebuilder:default=75
	TargetCPUUtilizationPercentage *int32 `json:"targetCPUUtilizationPercentage,omitempty"`
}

// ClusterDatabaseSpec configures OpenVox DB (PuppetDB) and its PostgreSQL backend.
type ClusterDatabaseSpec struct {
	// Enabled deploys OpenVox DB (PuppetDB) and connects compilers to it.
	// +kubebuilder:default=true
	Enabled *bool `json:"enabled,omitempty"`

	// Image is the OpenVox DB container image. When empty, the operator uses the OpenVoxDB version it was
	// released with (ghcr.io/yboikov/openvox-db:<version>, see internal/controller/versions.go).
	// +optional
	Image string `json:"image,omitempty"`

	// Replicas is the number of OpenVox DB pods.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	Replicas *int32 `json:"replicas,omitempty"`

	// Postgres configures either an external PostgreSQL cluster (CNPG/RDS/Crunchy) or a built-in StatefulSet.
	// +optional
	Postgres ClusterPostgresSpec `json:"postgres,omitempty"`

	// Housekeeping configures automatic PuppetDB node and report TTL cleanup.
	// +optional
	Housekeeping DatabaseHousekeepingSpec `json:"housekeeping,omitempty"`

	// CertificateAllowlist lists additional certnames (certificate CNs) allowed to use the OpenVox DB API over mTLS, e.g. a reporting tool. The compilers and the operator are always allowed; agent certificates are not.
	// +optional
	CertificateAllowlist []string `json:"certificateAllowlist,omitempty"`
}

// ClusterPostgresSpec configures PostgreSQL for OpenVox DB.
// Use mode "external" (CloudNativePG, RDS, Crunchy, ...) for production. Mode "builtin" runs a single
// PostgreSQL pod on ephemeral storage and is meant for development and demos only.
// +kubebuilder:validation:XValidation:rule="self.mode != 'external' || (has(self.host) && self.host != '' && has(self.credentialsSecretRef) && self.credentialsSecretRef != '')",message="postgres mode external requires host and credentialsSecretRef"
type ClusterPostgresSpec struct {
	// Mode selects "external" (CNPG/RDS/Crunchy) or "builtin" (ephemeral single-pod StatefulSet, dev/demo only).
	// +kubebuilder:validation:Enum=external;builtin
	// +kubebuilder:default="builtin"
	Mode string `json:"mode,omitempty"`

	// Host is the external PostgreSQL hostname when mode is "external" (e.g. "<cnpg-cluster>-rw.<namespace>.svc").
	// +optional
	Host string `json:"host,omitempty"`

	// Port is the PostgreSQL port.
	// +kubebuilder:default=5432
	Port int32 `json:"port,omitempty"`

	// DatabaseName is the PostgreSQL database name.
	// +kubebuilder:default="openvoxdb"
	DatabaseName string `json:"databaseName,omitempty"`

	// CredentialsSecretRef names a Secret with "username" and "password" keys
	// (a CloudNativePG "<cluster>-app" Secret works as-is). Required when mode is "external".
	// When omitted in "builtin" mode the operator generates "<cluster>-postgres-credentials".
	// +optional
	CredentialsSecretRef string `json:"credentialsSecretRef,omitempty"`

	// SSLMode is the PostgreSQL JDBC sslmode used when mode is "external". Ignored for "builtin" (no TLS).
	// +kubebuilder:validation:Enum=disable;allow;prefer;require;verify-ca;verify-full
	// +kubebuilder:default="require"
	SSLMode string `json:"sslMode,omitempty"`

	// Image is the PostgreSQL image when mode is "builtin".
	// +kubebuilder:default="quay.io/sclorg/postgresql-16-c9s:latest"
	Image string `json:"image,omitempty"`
}

// DatabaseHousekeepingSpec configures PuppetDB database.ini TTL settings.
type DatabaseHousekeepingSpec struct {
	// NodeTTL auto-deactivates nodes that have not checked in within this duration.
	// +kubebuilder:default="7d"
	NodeTTL string `json:"nodeTTL,omitempty"`

	// NodePurgeTTL permanently purges deactivated nodes and their certs after this duration.
	// +kubebuilder:default="14d"
	NodePurgeTTL string `json:"nodePurgeTTL,omitempty"`

	// ReportTTL purges old run reports after this duration.
	// +kubebuilder:default="14d"
	ReportTTL string `json:"reportTTL,omitempty"`

	// GCInterval controls how often PuppetDB runs garbage collection in minutes.
	// +kubebuilder:default=60
	GCInterval int32 `json:"gcInterval,omitempty"`
}

// ClusterNetworkingSpec configures how the compilers and the CA are exposed.
// Both endpoints serve the same service certificate, so every hostname listed in either block is added to its SANs.
type ClusterNetworkingSpec struct {
	// Server exposes the compiler pool (Service "<cluster>", Route "<cluster>"). Agents use it as "server".
	// +optional
	Server EndpointNetworkingSpec `json:"server,omitempty"`

	// CA exposes the certificate authority (Service "<cluster>-ca", Route "<cluster>-ca"). Agents outside the
	// Kubernetes cluster use it as "ca_server".
	// +optional
	CA EndpointNetworkingSpec `json:"ca,omitempty"`
}

// EndpointNetworkingSpec configures the exposure of one endpoint (compilers or CA).
type EndpointNetworkingSpec struct {
	// ExternalHostname is the DNS name agents use to reach this endpoint (added to the certificate SANs).
	// +optional
	ExternalHostname string `json:"externalHostname,omitempty"`

	// AdditionalDNSNames are extra DNS SANs added to the certificate.
	// +optional
	AdditionalDNSNames []string `json:"additionalDNSNames,omitempty"`

	// ServiceType is the Kubernetes Service type of this endpoint.
	// +kubebuilder:validation:Enum=ClusterIP;NodePort;LoadBalancer
	// +optional
	ServiceType corev1.ServiceType `json:"serviceType,omitempty"`

	// OpenShiftRoute configures an OpenShift Route (route.openshift.io/v1) with TLS passthrough to this endpoint.
	// +optional
	OpenShiftRoute *OpenShiftRouteSpec `json:"openShiftRoute,omitempty"`
}

// OpenShiftRouteSpec configures an OpenShift TLS passthrough Route.
type OpenShiftRouteSpec struct {
	// Enabled creates a route.openshift.io/v1 Route with TLS passthrough to the endpoint's Service.
	// Disabling it deletes the Route.
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`

	// Host is the Route hostname (defaults to the endpoint's externalHostname). It is added to the certificate SANs.
	// +optional
	Host string `json:"host,omitempty"`

	// Annotations are additional annotations added to the OpenShift Route.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}

// HieraSecretsSpec configures the native ESO and cert-manager to Hiera Bridge.
type HieraSecretsSpec struct {
	// Enabled activates the Hiera Secret & Certificate Bridge.
	// +kubebuilder:default=true
	Enabled *bool `json:"enabled,omitempty"`

	// LabelSelector matches Secrets in the namespace to inject into Hiera (defaults to voxkube.io/hiera-secret=true).
	// +optional
	LabelSelector map[string]string `json:"labelSelector,omitempty"`

	// CommonSecrets lists explicit Secret names whose keys are injected into common.yaml.
	// +optional
	CommonSecrets []string `json:"commonSecrets,omitempty"`
}

// ClusterWorkflowsSpec configures standard enterprise Puppet operational workflows.
type ClusterWorkflowsSpec struct {
	// NodeClassification configures the Kubernetes-native ENC and optional external ENC webhook.
	// +optional
	NodeClassification *WorkflowNodeClassificationSpec `json:"nodeClassification,omitempty"`

	// Facts configures custom and external trusted facts.
	// +optional
	Facts *WorkflowFactsSpec `json:"facts,omitempty"`

	// Reports configures Puppet run report processors and optional external webhooks.
	// +optional
	Reports *WorkflowReportsSpec `json:"reports,omitempty"`

	// Maintenance configures global No-Op mode and scheduled maintenance freeze windows.
	// +optional
	Maintenance *WorkflowMaintenanceSpec `json:"maintenance,omitempty"`
}

// WorkflowNodeClassificationSpec configures ENC behavior.
// +kubebuilder:validation:XValidation:rule="self.mode != 'externalHttp' || (has(self.externalURL) && self.externalURL != '')",message="mode externalHttp requires externalURL"
type WorkflowNodeClassificationSpec struct {
	// Mode selects the ENC: "voxnode" classifies from VoxNode resources; "externalHttp" uses the VoxNode
	// classification when one exists and otherwise GETs "<externalURL>/<certname>" (which must return ENC YAML);
	// "none" disables the ENC entirely (classification comes from your site.pp / Hiera only).
	// +kubebuilder:validation:Enum=voxnode;externalHttp;none
	// +kubebuilder:default="voxnode"
	Mode string `json:"mode,omitempty"`

	// DefaultEnvironment is the Puppet environment returned for nodes without a VoxNode classification.
	// +kubebuilder:default="production"
	DefaultEnvironment string `json:"defaultEnvironment,omitempty"`

	// AutoCreateVoxNode makes the operator create a VoxNode for every certname the CA knows about that has none:
	// pending CSRs get state PendingApproval, already-signed certificates get state Active.
	// +kubebuilder:default=true
	AutoCreateVoxNode *bool `json:"autoCreateVoxNode,omitempty"`

	// ExternalURL is the base URL of the fallback ENC when mode is "externalHttp".
	// +optional
	ExternalURL string `json:"externalURL,omitempty"`
}

// WorkflowFactsSpec configures Kubernetes metadata exposed to Puppet code.
type WorkflowFactsSpec struct {
	// InjectKubernetesMetadata adds the top-scope ENC parameters voxkube_cluster, voxkube_namespace and
	// voxkube_node (the VoxNode name) to every node's classification.
	// +kubebuilder:default=true
	InjectKubernetesMetadata bool `json:"injectKubernetesMetadata,omitempty"`
}

// WorkflowReportsSpec configures Puppet report processors.
type WorkflowReportsSpec struct {
	// Processors is the list of Puppet report processors (puppet.conf "reports").
	// Defaults to ["puppetdb"] when the database is enabled, otherwise ["store"].
	// +optional
	Processors []string `json:"processors,omitempty"`

	// WebhookURL enables Puppet's built-in "http" report processor and sends every report to this URL
	// (puppet.conf "reporturl").
	// +optional
	WebhookURL string `json:"webhookURL,omitempty"`
}

// WorkflowMaintenanceSpec configures global No-Op mode.
type WorkflowMaintenanceSpec struct {
	// GlobalNoop adds the top-scope ENC parameter "noop: true" to every classified node. Puppet does not act on
	// this variable by itself: your code must honour it (for example with the noop() function from trlinkin/noop).
	// +kubebuilder:default=false
	GlobalNoop bool `json:"globalNoop,omitempty"`
}

// VoxClusterSpec defines the desired state of a VoxCluster.
type VoxClusterSpec struct {
	// Code configures how Puppet environments and modules are deployed (Git/g10k, PVC, or ImageVolume).
	// +optional
	Code ClusterCodeSpec `json:"code,omitempty"`

	// CA configures the OpenVox Certificate Authority.
	// +optional
	CA ClusterCASpec `json:"ca,omitempty"`

	// Server configures the OpenVox catalog compiler pool.
	// +optional
	Server ClusterServerSpec `json:"server,omitempty"`

	// Database configures OpenVox DB (PuppetDB) and PostgreSQL.
	// +optional
	Database ClusterDatabaseSpec `json:"database,omitempty"`

	// Networking configures external Services, DNS SANs, and OpenShift Routes.
	// +optional
	Networking ClusterNetworkingSpec `json:"networking,omitempty"`

	// HieraSecrets configures the ESO & cert-manager to Hiera Secret Bridge.
	// +optional
	HieraSecrets HieraSecretsSpec `json:"hieraSecrets,omitempty"`

	// Workflows configures ENC, facts, report forwarding, and maintenance windows.
	// +optional
	Workflows ClusterWorkflowsSpec `json:"workflows,omitempty"`
}

// VoxClusterStatus defines the observed state of a VoxCluster.
type VoxClusterStatus struct {
	// Phase is the current high-level lifecycle phase of the cluster.
	// +optional
	Phase VoxClusterPhase `json:"phase,omitempty"`

	// ObservedGeneration is the metadata.generation last processed by the operator.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ServerURL is the external or cluster-internal URL where Puppet agents connect.
	// +optional
	ServerURL string `json:"serverURL,omitempty"`

	// CAURL is the URL of the dedicated OpenVox CA service.
	// +optional
	CAURL string `json:"caURL,omitempty"`

	// ServerExternalURL is the URL agents outside the Kubernetes cluster use for "server", when exposed.
	// +optional
	ServerExternalURL string `json:"serverExternalURL,omitempty"`

	// CAExternalURL is the URL agents outside the Kubernetes cluster use for "ca_server", when exposed.
	// +optional
	CAExternalURL string `json:"caExternalURL,omitempty"`

	// DatabaseURL is the internal URL of OpenVox DB (PuppetDB).
	// +optional
	DatabaseURL string `json:"databaseURL,omitempty"`

	// CASecretName is the name of the Secret containing the OpenVox CA certificate.
	// +optional
	CASecretName string `json:"caSecretName,omitempty"`

	// HieraSecretName is the name of the rendered Hiera Secret mounted into compilers.
	// +optional
	HieraSecretName string `json:"hieraSecretName,omitempty"`

	// ObservedCommit is the HEAD commit SHA of the control-repo last observed by the operator's Git poller.
	// +optional
	ObservedCommit string `json:"observedCommit,omitempty"`

	// ReadyReplicas is the number of ready compiler pods.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// Conditions represent the latest available observations of the VoxCluster state.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=vc
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Ready",type="integer",JSONPath=".status.readyReplicas"
// +kubebuilder:printcolumn:name="Commit",type="string",JSONPath=".status.observedCommit"
// +kubebuilder:printcolumn:name="Server URL",type="string",JSONPath=".status.serverURL"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// VoxCluster is the Schema for the voxclusters API.
type VoxCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VoxClusterSpec   `json:"spec,omitempty"`
	Status VoxClusterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// VoxClusterList contains a list of VoxCluster.
type VoxClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VoxCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &VoxCluster{}, &VoxClusterList{})
		return nil
	})
}
