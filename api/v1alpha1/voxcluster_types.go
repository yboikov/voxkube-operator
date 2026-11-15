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
type ClusterCodeSpec struct {
	// Mode selects the code deployment strategy.
	// +kubebuilder:validation:Enum=git;pvc;imageVolume
	// +kubebuilder:default="git"
	Mode string `json:"mode,omitempty"`

	// Git configures outbound-only central Git polling + synchronized g10k deployment.
	// +optional
	Git *GitCodeSpec `json:"git,omitempty"`

	// ClaimName is the PVC name when mode is "pvc".
	// +optional
	ClaimName string `json:"claimName,omitempty"`

	// Image is the container image reference when mode is "imageVolume".
	// +optional
	Image string `json:"image,omitempty"`
}

// GitCodeSpec configures outbound-only Git synchronization via g10k.
type GitCodeSpec struct {
	// Remote is the Git repository URL for the Puppet control-repo.
	Remote string `json:"remote"`

	// AuthType selects the Git authentication mechanism.
	// +kubebuilder:validation:Enum=githubApp;sshKey;httpsToken;none
	// +kubebuilder:default="githubApp"
	AuthType string `json:"authType,omitempty"`

	// GitHubApp configures native GitHub App authentication (RS256 JWT -> auto-rotated installation token).
	// +optional
	GitHubApp *GitHubAppAuthSpec `json:"githubApp,omitempty"`

	// CredentialsSecretRef references a Secret with ssh-privatekey (sshKey) or token/username/password (httpsToken).
	// +optional
	CredentialsSecretRef string `json:"credentialsSecretRef,omitempty"`

	// PollInterval is how often the operator polls the remote repository via git ls-remote.
	// +kubebuilder:default="60s"
	PollInterval string `json:"pollInterval,omitempty"`

	// SynchronizedRollout ensures all compiler pods switch to new commits atomically.
	// +kubebuilder:default=true
	SynchronizedRollout *bool `json:"synchronizedRollout,omitempty"`

	// GenerateTypes runs puppet generate types per environment after g10k sync for environment isolation.
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

	// TTL is the root CA certificate validity duration.
	// +kubebuilder:default="10y"
	TTL string `json:"ttl,omitempty"`

	// AutosignMode controls how incoming CSRs are signed.
	// +kubebuilder:validation:Enum=voxnode;psk;off;true
	// +kubebuilder:default="voxnode"
	AutosignMode string `json:"autosignMode,omitempty"`

	// PSKSecretRef references a Secret containing a shared passphrase when autosignMode is "psk".
	// +optional
	PSKSecretRef string `json:"pskSecretRef,omitempty"`
}

// ClusterServerSpec configures the OpenVox Server catalog compilers.
type ClusterServerSpec struct {
	// Image is the OpenVox Server container image.
	// +kubebuilder:default="ghcr.io/slauger/openvox-server:latest"
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

	// Image is the OpenVox DB container image.
	// +kubebuilder:default="ghcr.io/slauger/openvox-db:latest"
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
}

// ClusterPostgresSpec configures PostgreSQL for OpenVox DB.
type ClusterPostgresSpec struct {
	// Mode selects "external" (CNPG/RDS/Crunchy) or "builtin" (single-pod StatefulSet).
	// +kubebuilder:validation:Enum=external;builtin
	// +kubebuilder:default="builtin"
	Mode string `json:"mode,omitempty"`

	// Host is the external PostgreSQL hostname when mode is "external".
	// +optional
	Host string `json:"host,omitempty"`

	// Port is the PostgreSQL port.
	// +kubebuilder:default=5432
	Port int32 `json:"port,omitempty"`

	// DatabaseName is the PostgreSQL database name.
	// +kubebuilder:default="openvoxdb"
	DatabaseName string `json:"databaseName,omitempty"`

	// CredentialsSecretRef references a Secret with username and password keys.
	// +optional
	CredentialsSecretRef string `json:"credentialsSecretRef,omitempty"`

	// SSLMode is the PostgreSQL SSL connection mode.
	// +kubebuilder:default="require"
	SSLMode string `json:"sslMode,omitempty"`

	// Image is the PostgreSQL image when mode is "builtin".
	// +kubebuilder:default="quay.io/sclorg/postgresql-16-c9s:latest"
	Image string `json:"image,omitempty"`

	// StorageSize is the PVC size when mode is "builtin".
	// +kubebuilder:default="10Gi"
	StorageSize string `json:"storageSize,omitempty"`
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

// ClusterNetworkingSpec configures external exposure and DNS SANs for the OpenVox cluster.
type ClusterNetworkingSpec struct {
	// ExternalHostname is the primary DNS hostname Puppet agents use to connect (added to TLS SANs).
	// +optional
	ExternalHostname string `json:"externalHostname,omitempty"`

	// AdditionalDNSNames are extra DNS SANs included in the server TLS certificate.
	// +optional
	AdditionalDNSNames []string `json:"additionalDNSNames,omitempty"`

	// ServiceType is the Kubernetes Service type for the Puppet server pool.
	// +kubebuilder:validation:Enum=ClusterIP;NodePort;LoadBalancer
	// +kubebuilder:default="ClusterIP"
	ServiceType corev1.ServiceType `json:"serviceType,omitempty"`

	// OpenShiftRoute configures an OpenShift Route (route.openshift.io/v1) with TLS passthrough.
	// +optional
	OpenShiftRoute *OpenShiftRouteSpec `json:"openShiftRoute,omitempty"`
}

// OpenShiftRouteSpec configures an OpenShift TLS passthrough Route.
type OpenShiftRouteSpec struct {
	// Enabled creates a route.openshift.io/v1 Route pointing to the Puppet server Service.
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`

	// Host is the optional hostname for the OpenShift Route (defaults to spec.networking.externalHostname).
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
type WorkflowNodeClassificationSpec struct {
	// Mode selects "voxnode" (classify from VoxNode CRs), "externalHttp", or "none".
	// +kubebuilder:validation:Enum=voxnode;externalHttp;none
	// +kubebuilder:default="voxnode"
	Mode string `json:"mode,omitempty"`

	// DefaultEnvironment is the default Puppet environment when unspecified on a VoxNode.
	// +kubebuilder:default="production"
	DefaultEnvironment string `json:"defaultEnvironment,omitempty"`

	// AutoCreateVoxNode automatically creates a VoxNode CR when a new node checks in or submits a CSR.
	// +kubebuilder:default=true
	AutoCreateVoxNode *bool `json:"autoCreateVoxNode,omitempty"`

	// ExternalURL is the optional fallback ENC URL when mode is "externalHttp".
	// +optional
	ExternalURL string `json:"externalURL,omitempty"`
}

// WorkflowFactsSpec configures trusted facts injection.
type WorkflowFactsSpec struct {
	// InjectKubernetesMetadata injects cluster and environment metadata into node facts.
	// +kubebuilder:default=true
	InjectKubernetesMetadata bool `json:"injectKubernetesMetadata,omitempty"`
}

// WorkflowReportsSpec configures Puppet report processors.
type WorkflowReportsSpec struct {
	// Processors is the list of enabled Puppet report processors.
	// +optional
	Processors []string `json:"processors,omitempty"`

	// WebhookURL is an optional HTTP endpoint to forward Puppet run reports.
	// +optional
	WebhookURL string `json:"webhookURL,omitempty"`
}

// WorkflowMaintenanceSpec configures global No-Op mode.
type WorkflowMaintenanceSpec struct {
	// GlobalNoop forces all catalog compilations into noop mode during change freezes.
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

	// ServerURL is the external or cluster-internal URL where Puppet agents connect.
	// +optional
	ServerURL string `json:"serverURL,omitempty"`

	// DatabaseURL is the internal URL of OpenVox DB (PuppetDB).
	// +optional
	DatabaseURL string `json:"databaseURL,omitempty"`

	// CASecretName is the name of the Secret containing the OpenVox CA certificate.
	// +optional
	CASecretName string `json:"caSecretName,omitempty"`

	// HieraSecretName is the name of the rendered Hiera Secret mounted into compilers.
	// +optional
	HieraSecretName string `json:"hieraSecretName,omitempty"`

	// ObservedCommit is the latest Git commit SHA observed by the central Git poller.
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
