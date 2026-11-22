package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// VoxNodeState describes the desired lifecycle state of a Puppet node.
// +kubebuilder:validation:Enum=PendingApproval;Active;Revoked;Decommissioned
type VoxNodeState string

const (
	NodeStatePendingApproval VoxNodeState = "PendingApproval"
	NodeStateActive          VoxNodeState = "Active"
	NodeStateRevoked         VoxNodeState = "Revoked"
	NodeStateDecommissioned  VoxNodeState = "Decommissioned"
)

// VoxNodePhase describes the observed status phase of a VoxNode:
// PendingCSR (no CSR yet, or CSR awaiting approval), Signed (certificate signed, node not yet seen in OpenVox DB),
// Active (signed and reporting), Revoked, Decommissioned, Error.
// +kubebuilder:validation:Enum=PendingCSR;Signed;Active;Revoked;Decommissioned;Error
type VoxNodePhase string

const (
	NodePhasePendingCSR     VoxNodePhase = "PendingCSR"
	NodePhaseSigned         VoxNodePhase = "Signed"
	NodePhaseActive         VoxNodePhase = "Active"
	NodePhaseRevoked        VoxNodePhase = "Revoked"
	NodePhaseDecommissioned VoxNodePhase = "Decommissioned"
	NodePhaseError          VoxNodePhase = "Error"
)

// NodeClassificationSpec configures Kubernetes-native ENC (External Node Classifier) data for a node.
type NodeClassificationSpec struct {
	// Environment is the Puppet environment assigned to this node (e.g., "production", "staging").
	// When empty, the VoxCluster's spec.workflows.nodeClassification.defaultEnvironment is used ("production" by default).
	// +optional
	Environment string `json:"environment,omitempty"`

	// Role is the high-level role assigned to this node (e.g., "webserver", "database"), passed to Puppet as the
	// top-scope variable $role. When empty, the pp_role extension of the node's certificate is used.
	// Automatically unlocks Hiera secrets labeled with voxkube.io/hiera-role=<role>.
	// +optional
	Role string `json:"role,omitempty"`

	// Classes is the list of Puppet classes to include on this node.
	// +optional
	Classes []string `json:"classes,omitempty"`

	// Parameters are top-scope ENC parameters passed to Puppet for this node.
	// +optional
	Parameters map[string]apiextensionsv1.JSON `json:"parameters,omitempty"`
}

// CertManagerIssuerRef references a cert-manager Issuer or ClusterIssuer.
type CertManagerIssuerRef struct {
	// Name of the Issuer or ClusterIssuer.
	Name string `json:"name"`

	// Kind is either "Issuer" or "ClusterIssuer".
	// +kubebuilder:validation:Enum=Issuer;ClusterIssuer
	// +kubebuilder:default="ClusterIssuer"
	Kind string `json:"kind,omitempty"`

	// Group is the API group of the issuer (defaults to cert-manager.io).
	// +kubebuilder:default="cert-manager.io"
	Group string `json:"group,omitempty"`
}

// CertManagerCertRequest defines a cert-manager SSL certificate to issue and inject into Hiera for this VM.
type CertManagerCertRequest struct {
	// Name is the logical identifier for this certificate (e.g., "public_web", "internal_api").
	// Injected into Hiera under openvox::certs[name] and certs::<name>::tls_crt / tls_key.
	Name string `json:"name"`

	// IssuerRef references the cert-manager Issuer or ClusterIssuer used to sign this certificate.
	IssuerRef CertManagerIssuerRef `json:"issuerRef"`

	// DNSNames is the list of DNS SANs for the certificate.
	// +optional
	DNSNames []string `json:"dnsNames,omitempty"`

	// IPAddresses is the list of IP SANs for the certificate.
	// +optional
	IPAddresses []string `json:"ipAddresses,omitempty"`

	// Duration is the requested certificate validity period (e.g., "2160h" for 90d).
	// +kubebuilder:default="2160h"
	Duration string `json:"duration,omitempty"`

	// RenewBefore is how long before expiry cert-manager should renew the certificate.
	// +kubebuilder:default="720h"
	RenewBefore string `json:"renewBefore,omitempty"`

	// TargetCertPath is an optional file path on the VM where Puppet should place tls.crt.
	// +optional
	TargetCertPath string `json:"targetCertPath,omitempty"`

	// TargetKeyPath is an optional file path on the VM where Puppet should place tls.key.
	// +optional
	TargetKeyPath string `json:"targetKeyPath,omitempty"`

	// SecretName names the cert-manager Certificate and the TLS Secret it writes (default voxnode-cert-<voxnode>-<name>). The operator creates and owns that Certificate, so don't point this at a Secret something else manages.
	// +optional
	SecretName string `json:"secretName,omitempty"`
}

// NodeSecretRef maps a Kubernetes Secret (e.g. synced by External Secrets Operator) into this node's Hiera data.
type NodeSecretRef struct {
	// SecretName is the name of the Kubernetes Secret in the same namespace.
	SecretName string `json:"secretName"`

	// HieraPrefix is the Hiera key prefix under which the Secret's keys will be injected (e.g. "nginx::basic_auth").
	HieraPrefix string `json:"hieraPrefix"`
}

// VoxNodeSpec defines the desired state of a VoxNode.
type VoxNodeSpec struct {
	// ClusterRef is the name of the VoxCluster managing this node.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="clusterRef is immutable"
	ClusterRef string `json:"clusterRef"`

	// Certname is the Puppet agent certificate name (FQDN) of the VM/client.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="certname is immutable; create a new VoxNode for a new certname"
	// +kubebuilder:validation:XValidation:rule="!self.endsWith('.svc') && !self.endsWith('.svc.cluster.local')",message="certnames ending in .svc or .svc.cluster.local are reserved for the services of the VoxCluster"
	Certname string `json:"certname"`

	// State controls CSR signing, revocation, and atomic CA + PuppetDB decommissioning.
	// +kubebuilder:default="Active"
	State VoxNodeState `json:"state,omitempty"`

	// Classification defines the Kubernetes-native ENC classification for this node.
	// +optional
	Classification NodeClassificationSpec `json:"classification,omitempty"`

	// Certificates requests one or more cert-manager SSL certificates for this VM and injects them into Hiera.
	// +optional
	Certificates []CertManagerCertRequest `json:"certificates,omitempty"`

	// Secrets maps node-specific ESO / Kubernetes Secrets into this VM's Hiera hierarchy.
	// +optional
	Secrets []NodeSecretRef `json:"secrets,omitempty"`
}

// VoxNodeStatus defines the observed state of a VoxNode.
type VoxNodeStatus struct {
	// Phase is the current lifecycle phase of the node.
	// +optional
	Phase VoxNodePhase `json:"phase,omitempty"`

	// CertificateSigned indicates whether the node's Puppet agent CSR has been signed by the OpenVox CA.
	// +optional
	CertificateSigned bool `json:"certificateSigned,omitempty"`

	// CertificateFingerprint is the SHA256 fingerprint of the signed Puppet agent certificate.
	// +optional
	CertificateFingerprint string `json:"certificateFingerprint,omitempty"`

	// CertificateNotAfter is when the node's signed Puppet agent certificate expires, as reported by the OpenVox CA.
	// +optional
	CertificateNotAfter *metav1.Time `json:"certificateNotAfter,omitempty"`

	// CertificateExtensions are the Puppet extensions of the node's certificate (e.g. pp_role, pp_datacenter),
	// read from its pending CSR before signing and from the signed certificate afterwards. Registered extensions
	// appear by name, site-specific ones (1.3.6.1.4.1.34380.1.2.*) by OID. pp_preshared_key is never shown.
	// +optional
	CertificateExtensions map[string]string `json:"certificateExtensions,omitempty"`

	// Role is the role passed to Puppet as the top-scope variable $role: spec.classification.role when set,
	// otherwise the certificate's pp_role extension.
	// +optional
	Role string `json:"role,omitempty"`

	// CACleaned indicates whether the node's certificate has been revoked and cleaned from the CA.
	// +optional
	CACleaned bool `json:"caCleaned,omitempty"`

	// PuppetDBDeactivated indicates whether the node has been deactivated in OpenVox DB (PuppetDB).
	// +optional
	PuppetDBDeactivated bool `json:"puppetDBDeactivated,omitempty"`

	// ReadyCertificates is the count of cert-manager certificates ready and synced to Hiera for this node.
	// +optional
	ReadyCertificates int32 `json:"readyCertificates,omitempty"`

	// LastReportStatus is the status of the node's most recent Puppet run ("changed", "unchanged", "failed").
	// +optional
	LastReportStatus string `json:"lastReportStatus,omitempty"`

	// LastSeenTime is the timestamp when the node last checked into PuppetDB.
	// +optional
	LastSeenTime *metav1.Time `json:"lastSeenTime,omitempty"`

	// Conditions represent the latest available observations of the VoxNode state.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=vn
// +kubebuilder:printcolumn:name="Cluster",type="string",JSONPath=".spec.clusterRef"
// +kubebuilder:printcolumn:name="Certname",type="string",JSONPath=".spec.certname"
// +kubebuilder:printcolumn:name="Role",type="string",JSONPath=".status.role"
// +kubebuilder:printcolumn:name="State",type="string",JSONPath=".spec.state"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Certs",type="integer",JSONPath=".status.readyCertificates"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// VoxNode is the Schema for the voxnodes API.
type VoxNode struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VoxNodeSpec   `json:"spec,omitempty"`
	Status VoxNodeStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// VoxNodeList contains a list of VoxNode.
type VoxNodeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VoxNode `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &VoxNode{}, &VoxNodeList{})
		return nil
	})
}
