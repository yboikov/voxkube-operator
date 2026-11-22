package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	openvoxv1alpha1 "github.com/yboikov/voxkube-operator/api/v1alpha1"
)

const (
	// LabelAutoCreated marks VoxNodes created by the operator from CA state.
	LabelAutoCreated = "voxkube.io/auto-created"

	autosignVoxNode = "voxnode"
	autosignOff     = "off"
	autosignTrue    = "true"
	autosignPSK     = "psk"

	caDir         = "/var/cache/puppetlabs/ca"
	pskMountPath  = "/etc/puppetlabs/autosign-psk"
	caScriptsPath = "/etc/puppetlabs/ca-scripts"
)

func caEnabled(cluster *openvoxv1alpha1.VoxCluster) bool {
	return cluster.Spec.CA.Enabled == nil || *cluster.Spec.CA.Enabled
}

func autosignMode(cluster *openvoxv1alpha1.VoxCluster) string {
	if cluster.Spec.CA.AutosignMode == "" {
		return autosignVoxNode
	}
	return cluster.Spec.CA.AutosignMode
}

func autoCreateVoxNodesEnabled(cluster *openvoxv1alpha1.VoxCluster) bool {
	nc := cluster.Spec.Workflows.NodeClassification
	return nc == nil || nc.AutoCreateVoxNode == nil || *nc.AutoCreateVoxNode
}

// pskAutosignScript is Puppet's policy-based autosign executable for autosignMode "psk", in Puppet's own Ruby.
// Puppet passes the certname as ARGV[0] and the CSR PEM on stdin; exit 0 signs the CSR. The CSR is decoded as
// ASN.1 (no scraping of "openssl req -text"), its signature is checked, and the key is accepted from the
// challengePassword attribute (csr_attributes.yaml custom_attributes) or the pp_preshared_key extension request
// (extension_requests), compared in constant time. Any error denies.
const pskAutosignScript = `#!/opt/puppetlabs/puppet/bin/ruby
# Rendered by voxkube-operator.
require 'openssl'

PP_PRESHARED_KEY = '1.3.6.1.4.1.34380.1.1.4'

def offered_keys(csr)
  keys = []
  csr.attributes.each do |attr|
    case attr.oid
    when 'challengePassword', '1.2.840.113549.1.9.7'
      attr.value.value.each { |v| keys << v.value.to_s }
    when 'extReq', 'msExtReq', '1.2.840.113549.1.9.14', '1.3.6.1.4.1.311.2.1.14'
      attr.value.value.each do |exts|
        exts.value.each do |ext|
          parts = ext.value
          next unless parts.first.oid == PP_PRESHARED_KEY
          raw = parts.last.value
          inner = begin
            OpenSSL::ASN1.decode(raw).value
          rescue OpenSSL::ASN1::ASN1Error
            raw
          end
          keys << inner.to_s
        end
      end
    end
  end
  keys
end

begin
  certname = ARGV[0].to_s
  # In-cluster service names are reserved: OpenVox DB trusts the service certname.
  exit 1 if certname.empty? || certname.end_with?('.svc') || certname.end_with?('.svc.cluster.local')

  psk_file = '` + pskMountPath + `/psk'
  expected = File.file?(psk_file) ? File.read(psk_file).strip : ''
  exit 1 if expected.empty?

  csr = OpenSSL::X509::Request.new($stdin.read)
  exit 1 unless csr.verify(csr.public_key)

  ok = offered_keys(csr).any? { |k| !k.empty? && OpenSSL.secure_compare(k, expected) }
  exit(ok ? 0 : 1)
rescue StandardError
  exit 1
end
`

// reconcileCAWorkload renders the dedicated CA StatefulSet, its config, Service and PVC.
func (r *VoxClusterReconciler) reconcileCAWorkload(ctx context.Context, cluster *openvoxv1alpha1.VoxCluster, st *clusterState) error {
	st.caEnabled = caEnabled(cluster)
	if !st.caEnabled {
		return nil
	}

	caName := fmt.Sprintf("%s-ca", cluster.Name)
	pvcName := fmt.Sprintf("%s-ca-data", cluster.Name)
	if err := r.ensureCAPVC(ctx, cluster, pvcName); err != nil {
		return err
	}

	mode := autosignMode(cluster)
	autosign := "false"
	switch mode {
	case autosignTrue:
		autosign = "true"
	case autosignPSK:
		if cluster.Spec.CA.PSKSecretRef == "" {
			return fmt.Errorf("spec.ca.autosignMode is \"psk\" but spec.ca.pskSecretRef is empty")
		}
		autosign = caScriptsPath + "/autosign-psk.sh"
	}

	certname := serviceCertname(cluster)
	caCMName := fmt.Sprintf("%s-ca-config", cluster.Name)
	webserverConf := `webserver: {
    access-log-config: /etc/puppetlabs/puppetserver/request-logging.xml
    client-auth: want
    ssl-host: 0.0.0.0
    ssl-port: 8140
    ssl-cert: /var/cache/puppetlabs/ssldir/certs/` + certname + `.pem
    ssl-key: /var/cache/puppetlabs/ssldir/private_keys/` + certname + `.pem
    ssl-ca-cert: ` + caDir + `/ca_crt.pem
    ssl-crl-path: ` + caDir + `/ca_crl.pem
}
`
	caCfg := `puppetlabs.services.ca.certificate-authority-service/certificate-authority-service
puppetlabs.trapperkeeper.services.watcher.filesystem-watch-service/filesystem-watch-service
`
	puppetConf := fmt.Sprintf(`[main]
certname = %s
confdir = /etc/puppetlabs/puppet
codedir = /etc/puppetlabs/code
vardir = /var/cache/puppetlabs/vardir
logdir = /var/log/puppetlabs/puppetserver
rundir = /var/run/puppetlabs/puppetserver
ssldir = /var/cache/puppetlabs/ssldir
cadir = %s
autosign = %s
`, certname, caDir, autosign)

	puppetserverConf := puppetserverConfig(1)
	bootstrapScript := fmt.Sprintf(`#!/bin/sh
set -e
CADIR=%s
SSLDIR=/var/cache/puppetlabs/ssldir
VARDATA=/var/cache/puppetlabs/vardir
CERTNAME=%q

mkdir -p "$CADIR/requests" "$CADIR/signed" "$CADIR/private"
mkdir -p "$SSLDIR/certs" "$SSLDIR/private_keys" "$SSLDIR/public_keys" "$SSLDIR/certificate_requests" "$SSLDIR/private"
mkdir -p "$VARDATA"
chmod 0750 "$CADIR/private" "$SSLDIR/private_keys" "$SSLDIR/private"

# Disaster-recovery safe: only seed CA files if ca_crt.pem does not already exist on the PVC.
if [ ! -f "$CADIR/ca_crt.pem" ]; then
  cp /etc/puppetlabs/puppet/ssl-certs/ca.crt "$CADIR/ca_crt.pem"
  cp /etc/puppetlabs/puppet/ssl-certs/ca.key "$CADIR/ca_key.pem"
  cp /etc/puppetlabs/puppet/ssl-certs/ca.pub "$CADIR/ca_pub.pem"
  cp /etc/puppetlabs/puppet/ssl-certs/crl.pem "$CADIR/ca_crl.pem"
  cp /etc/puppetlabs/puppet/ssl-certs/crl.pem "$CADIR/infra_crl.pem"
  printf "0002" > "$CADIR/serial"
  : > "$CADIR/inventory.txt"
  : > "$CADIR/infra_inventory.txt"
  : > "$CADIR/infra_serials"
  chmod 0640 "$CADIR/ca_key.pem"
fi

# The CA on the PVC must be the one in the CA Secret (which signs the service certificate). They differ when the
# PVC and the Secret were restored from different backups or the Secret was regenerated: compilers would then
# trust a CA that doesn't sign anything. Refuse to start instead of serving a split CA.
# Compare the certificates' public keys (stable across CA certificate renewal); without openssl, the key files.
if command -v openssl >/dev/null 2>&1; then
  pvc_key=$(openssl x509 -noout -pubkey -in "$CADIR/ca_crt.pem")
  secret_key=$(openssl x509 -noout -pubkey -in /etc/puppetlabs/puppet/ssl-certs/ca.crt)
else
  pvc_key=$(cat "$CADIR/ca_pub.pem")
  secret_key=$(cat /etc/puppetlabs/puppet/ssl-certs/ca.pub)
fi
if [ "$pvc_key" != "$secret_key" ]; then
  echo "FATAL: the CA key on the PVC ($CADIR) does not match the CA Secret. Restore the CA Secret %s together with the PVC it belongs to." >&2
  exit 1
fi

cp "$CADIR/ca_crt.pem" "$SSLDIR/certs/ca.pem"
cp "$CADIR/ca_crl.pem" "$SSLDIR/crl.pem"
cp /etc/puppetlabs/puppet/ssl-certs/tls.crt "$SSLDIR/certs/${CERTNAME}.pem"
cp /etc/puppetlabs/puppet/ssl-certs/tls.key "$SSLDIR/private_keys/${CERTNAME}.pem"
cp /etc/puppetlabs/puppet/ssl-certs/tls.pub "$SSLDIR/public_keys/${CERTNAME}.pem"
chmod 0640 "$SSLDIR/private_keys/${CERTNAME}.pem"
`, caDir, certname, caSecretName(cluster))

	caCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      caCMName,
			Namespace: cluster.Namespace,
			Labels:    voxClusterLabels(cluster.Name, "ca"),
		},
		Data: map[string]string{
			"webserver.conf":    webserverConf,
			"ca.cfg":            caCfg,
			"puppet.conf":       puppetConf,
			"puppetserver.conf": puppetserverConf,
			"bootstrap-ca.sh":   bootstrapScript,
			"autosign-psk.sh":   pskAutosignScript,
		},
	}
	if err := controllerutil.SetControllerReference(cluster, caCM, r.Scheme); err != nil {
		return err
	}
	if err := r.createOrUpdateConfigMap(ctx, caCM); err != nil {
		return err
	}

	img := serverImage(cluster)
	scriptMode := int32(0o755)
	volumes := []corev1.Volume{
		{Name: "ca-tls", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: st.caSecretName}}},
		{Name: "ca-config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: caCMName},
		}}},
		{Name: "ca-scripts", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: caCMName},
			DefaultMode:          &scriptMode,
			Items: []corev1.KeyToPath{
				{Key: "bootstrap-ca.sh", Path: "bootstrap-ca.sh"},
				{Key: "autosign-psk.sh", Path: "autosign-psk.sh"},
			},
		}}},
		{Name: "ca-data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvcName}}},
		{Name: "ca-log", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "ca-run", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "ca-code", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
	mounts := []corev1.VolumeMount{
		{Name: "ca-tls", MountPath: "/etc/puppetlabs/puppet/ssl-certs", ReadOnly: true},
		{Name: "ca-config", MountPath: "/etc/puppetlabs/puppetserver/conf.d/webserver.conf", SubPath: "webserver.conf", ReadOnly: true},
		{Name: "ca-config", MountPath: "/etc/puppetlabs/puppetserver/conf.d/puppetserver.conf", SubPath: "puppetserver.conf", ReadOnly: true},
		{Name: "ca-config", MountPath: "/etc/puppetlabs/puppetserver/services.d/ca.cfg", SubPath: "ca.cfg", ReadOnly: true},
		{Name: "ca-config", MountPath: "/etc/puppetlabs/puppet/puppet.conf", SubPath: "puppet.conf", ReadOnly: true},
		{Name: "ca-scripts", MountPath: caScriptsPath, ReadOnly: true},
		{Name: "ca-data", MountPath: "/var/cache/puppetlabs"},
		{Name: "ca-log", MountPath: "/var/log/puppetlabs"},
		{Name: "ca-run", MountPath: "/var/run/puppetlabs"},
		{Name: "ca-code", MountPath: "/etc/puppetlabs/code"},
	}
	if mode == autosignPSK {
		volumes = append(volumes, corev1.Volume{Name: "autosign-psk", VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: cluster.Spec.CA.PSKSecretRef},
		}})
		mounts = append(mounts, corev1.VolumeMount{Name: "autosign-psk", MountPath: pskMountPath, ReadOnly: true})
	}

	caSTS := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      caName,
			Namespace: cluster.Namespace,
			Labels:    voxClusterLabels(cluster.Name, "ca"),
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: caName,
			Replicas:    new(int32(1)),
			Selector:    &metav1.LabelSelector{MatchLabels: voxClusterLabels(cluster.Name, "ca")},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: voxClusterLabels(cluster.Name, "ca"),
					Annotations: map[string]string{
						AnnotationConfigHash: hashStrings(caCM.Data),
						AnnotationTLSHash:    st.tlsHash,
					},
				},
				Spec: corev1.PodSpec{
					SecurityContext: buildOpenShiftPodSecurityContext(cluster.Spec.Server.SecurityContext),
					InitContainers: []corev1.Container{{
						Name:            "ca-init",
						Image:           img,
						ImagePullPolicy: cluster.Spec.Server.ImagePullPolicy,
						Command:         []string{"/bin/sh", caScriptsPath + "/bootstrap-ca.sh"},
						SecurityContext: buildRestrictedContainerSecurityContext(),
						VolumeMounts: []corev1.VolumeMount{
							{Name: "ca-tls", MountPath: "/etc/puppetlabs/puppet/ssl-certs", ReadOnly: true},
							{Name: "ca-scripts", MountPath: caScriptsPath, ReadOnly: true},
							{Name: "ca-data", MountPath: "/var/cache/puppetlabs"},
						},
					}},
					Containers: []corev1.Container{{
						Name:            "openvox-ca",
						Image:           img,
						ImagePullPolicy: cluster.Spec.Server.ImagePullPolicy,
						SecurityContext: buildRestrictedContainerSecurityContext(),
						Env:             []corev1.EnvVar{{Name: "JAVA_ARGS", Value: caJavaArgs(cluster)}},
						Ports:           []corev1.ContainerPort{{Name: "puppet-ca", ContainerPort: 8140, Protocol: corev1.ProtocolTCP}},
						ReadinessProbe:  puppetStatusProbe(8140, corev1.URISchemeHTTPS),
						Resources:       cluster.Spec.CA.Resources,
						VolumeMounts:    mounts,
					}},
					Volumes: volumes,
				},
			},
		},
	}
	withProbes(&caSTS.Spec.Template.Spec.Containers[0])
	applyPlacement(&caSTS.Spec.Template.Spec, cluster, nil)
	if err := controllerutil.SetControllerReference(cluster, caSTS, r.Scheme); err != nil {
		return err
	}
	if err := r.createOrUpdateStatefulSet(ctx, caSTS); err != nil {
		return err
	}

	caSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      caName,
			Namespace: cluster.Namespace,
			Labels:    voxClusterLabels(cluster.Name, "ca"),
		},
		Spec: corev1.ServiceSpec{
			Type:     serviceTypeOrDefault(cluster.Spec.Networking.CA.ServiceType),
			Selector: voxClusterLabels(cluster.Name, "ca"),
			Ports:    []corev1.ServicePort{{Name: "https-ca", Port: 8140, TargetPort: intstr.FromInt32(8140), Protocol: corev1.ProtocolTCP}},
		},
	}
	if err := controllerutil.SetControllerReference(cluster, caSvc, r.Scheme); err != nil {
		return err
	}
	if err := r.createOrUpdateService(ctx, caSvc); err != nil {
		return err
	}

	st.caURL = fmt.Sprintf("https://%s.%s.svc.cluster.local:8140", caName, cluster.Namespace)
	current := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: caName}, current); err == nil {
		st.caReady = current.Status.ReadyReplicas > 0
	}
	return nil
}

// ensureCAPVC creates the CA PVC once. It is intentionally not owned by the VoxCluster: the CA must survive
// deletion and re-creation of the VoxCluster.
func (r *VoxClusterReconciler) ensureCAPVC(ctx context.Context, cluster *openvoxv1alpha1.VoxCluster, pvcName string) error {
	existing := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: pvcName}, existing)
	if err == nil {
		refs := existing.OwnerReferences[:0]
		for _, ref := range existing.OwnerReferences {
			if ref.UID != cluster.UID {
				refs = append(refs, ref)
			}
		}
		if len(refs) != len(existing.OwnerReferences) {
			existing.OwnerReferences = refs
			return r.Update(ctx, existing)
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	storageSize := cluster.Spec.CA.StorageSize
	if storageSize == "" {
		storageSize = "2Gi"
	}
	qty, err := resource.ParseQuantity(storageSize)
	if err != nil {
		return fmt.Errorf("invalid spec.ca.storageSize %q: %w", storageSize, err)
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        pvcName,
			Namespace:   cluster.Namespace,
			Labels:      voxClusterLabels(cluster.Name, "ca"),
			Annotations: map[string]string{caSecretRetainNote: "true"},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: qty},
			},
		},
	}
	if cluster.Spec.CA.StorageClassName != "" {
		sc := cluster.Spec.CA.StorageClassName
		pvc.Spec.StorageClassName = &sc
	}
	return r.Create(ctx, pvc)
}

var invalidNameChars = regexp.MustCompile(`[^a-z0-9.-]+`)

// voxNodeNameForCertname turns a certname into a valid Kubernetes object name.
func voxNodeNameForCertname(certname string) string {
	name := invalidNameChars.ReplaceAllString(strings.ToLower(certname), "-")
	name = strings.Trim(name, "-.")
	if len(name) > 240 {
		name = name[:240]
	}
	if name == "" {
		name = "node"
	}
	return name
}

// autoCreateVoxNodes creates a VoxNode for every certname known to the CA that has none yet:
// pending CSRs become PendingApproval, signed certificates become Active.
func (r *VoxClusterReconciler) autoCreateVoxNodes(ctx context.Context, cluster *openvoxv1alpha1.VoxCluster, caClient OpenVoxCAClient, caURL string) error {
	statuses, err := caClient.ListCertStatuses(ctx, caURL)
	if err != nil {
		return err
	}
	nodes := &openvoxv1alpha1.VoxNodeList{}
	if err := r.List(ctx, nodes, client.InNamespace(cluster.Namespace)); err != nil {
		return err
	}
	known := map[string]bool{}
	for _, n := range nodes.Items {
		// Only this cluster's VoxNodes count: another VoxCluster in the namespace may manage the same certname.
		if n.Spec.ClusterRef == cluster.Name {
			known[n.Spec.Certname] = true
		}
	}

	serverCert := serviceCertname(cluster)
	for _, cs := range statuses {
		if cs.Name == "" || cs.Name == serverCert || known[cs.Name] {
			continue
		}
		var state openvoxv1alpha1.VoxNodeState
		switch cs.State {
		case "requested":
			state = openvoxv1alpha1.NodeStatePendingApproval
		case "signed":
			state = openvoxv1alpha1.NodeStateActive
		default:
			continue
		}

		name := voxNodeNameForCertname(cs.Name)
		probe := &openvoxv1alpha1.VoxNode{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: name}, probe); err == nil {
			sum := sha256.Sum256([]byte(cs.Name))
			name = fmt.Sprintf("%s-%s", name, hex.EncodeToString(sum[:])[:6])
		} else if !apierrors.IsNotFound(err) {
			return err
		}

		vn := &openvoxv1alpha1.VoxNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: cluster.Namespace,
				Labels:    map[string]string{LabelAutoCreated: "true"},
			},
			Spec: openvoxv1alpha1.VoxNodeSpec{
				ClusterRef: cluster.Name,
				Certname:   cs.Name,
				State:      state,
			},
		}
		if err := r.Create(ctx, vn); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
		known[cs.Name] = true
		log.FromContext(ctx).Info("auto-created VoxNode from CA state", "voxnode", name, "certname", cs.Name, "state", state)
		r.event(cluster, corev1.EventTypeNormal, "VoxNodeCreated", fmt.Sprintf("Created VoxNode %s for %s (%s)", name, cs.Name, state))
	}
	return nil
}

// puppetStatusProbe is a readiness probe against the trapperkeeper status service.
func puppetStatusProbe(port int32, scheme corev1.URIScheme) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path:   "/status/v1/simple",
				Port:   intstr.FromInt32(port),
				Scheme: scheme,
			},
		},
		InitialDelaySeconds: 20,
		PeriodSeconds:       10,
		TimeoutSeconds:      5,
		FailureThreshold:    6,
	}
}

// localStatusProbe checks /status/v1/simple on a plain-HTTP port bound to 127.0.0.1 inside the container, which an
// httpGet probe (sent by the kubelet to the pod IP) can't reach.
func localStatusProbe(port int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{Command: []string{
				"curl", "-fsS", "-o", "/dev/null", "--max-time", "4", fmt.Sprintf("http://127.0.0.1:%d/status/v1/simple", port),
			}},
		},
		InitialDelaySeconds: 20,
		PeriodSeconds:       10,
		TimeoutSeconds:      5,
		FailureThreshold:    6,
	}
}

func serverImage(cluster *openvoxv1alpha1.VoxCluster) string {
	if cluster.Spec.Server.Image != "" {
		return cluster.Spec.Server.Image
	}
	return DefaultOpenVoxServerImage
}

func puppetserverConfig(maxActive int32) string {
	return fmt.Sprintf(`jruby-puppet: {
    ruby-load-path: [/opt/puppetlabs/puppet/lib/ruby/vendor_ruby]
    gem-home: /opt/puppetlabs/server/data/puppetserver/jruby-gems
    gem-path: [${jruby-puppet.gem-home}, "/opt/puppetlabs/server/data/puppetserver/vendored-jruby-gems", "/opt/puppetlabs/puppet/lib/ruby/vendor_gems"]
    master-conf-dir: /etc/puppetlabs/puppet
    master-code-dir: /etc/puppetlabs/code
    master-var-dir: /var/cache/puppetlabs/vardir
    master-run-dir: /var/run/puppetlabs/puppetserver
    master-log-dir: /var/log/puppetlabs/puppetserver
    max-active-instances: %d
    max-requests-per-instance: 0
}
http-client: {
}
bolt: {
    projects-dir: "/etc/puppetlabs/code/projects"
}
dropsonde: {
    enabled: false
}
`, maxActive)
}

// caJavaArgs is spec.ca.javaArgs or the default CA heap.
func caJavaArgs(cluster *openvoxv1alpha1.VoxCluster) string {
	if cluster.Spec.CA.JavaArgs != "" {
		return cluster.Spec.CA.JavaArgs
	}
	return "-Xms256m -Xmx384m"
}
