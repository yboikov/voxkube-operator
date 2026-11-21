package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	openvoxv1alpha1 "github.com/yboikov/voxkube-operator/api/v1alpha1"
)

const (
	encModeVoxNode      = "voxnode"
	encModeExternalHTTP = "externalHttp"
	encModeNone         = "none"
)

func encMode(cluster *openvoxv1alpha1.VoxCluster) string {
	if nc := cluster.Spec.Workflows.NodeClassification; nc != nil && nc.Mode != "" {
		return nc.Mode
	}
	return encModeVoxNode
}

func encDefaultEnvironment(cluster *openvoxv1alpha1.VoxCluster) string {
	if cluster != nil {
		if nc := cluster.Spec.Workflows.NodeClassification; nc != nil && nc.DefaultEnvironment != "" {
			return nc.DefaultEnvironment
		}
	}
	return "production"
}

func injectMetadata(cluster *openvoxv1alpha1.VoxCluster) bool {
	if cluster == nil {
		return false
	}
	f := cluster.Spec.Workflows.Facts
	return f == nil || f.InjectKubernetesMetadata
}

func globalNoop(cluster *openvoxv1alpha1.VoxCluster) bool {
	return cluster != nil && cluster.Spec.Workflows.Maintenance != nil && cluster.Spec.Workflows.Maintenance.GlobalNoop
}

// RenderVoxNodeENCYAML renders the Puppet ENC YAML classification for a VoxNode.
func RenderVoxNodeENCYAML(node *openvoxv1alpha1.VoxNode, cluster *openvoxv1alpha1.VoxCluster) ([]byte, error) {
	env := node.Spec.Classification.Environment
	if env == "" {
		env = encDefaultEnvironment(cluster)
	}

	params := make(map[string]interface{})
	for k, rawJSON := range node.Spec.Classification.Parameters {
		if len(rawJSON.Raw) == 0 {
			continue
		}
		var val interface{}
		if err := json.Unmarshal(rawJSON.Raw, &val); err != nil {
			return nil, fmt.Errorf("unmarshaling ENC parameter %q: %w", k, err)
		}
		params[k] = val
	}
	if role := EffectiveRole(node); role != "" {
		params["role"] = role
	}
	if injectMetadata(cluster) {
		params["voxkube_cluster"] = cluster.Name
		params["voxkube_namespace"] = cluster.Namespace
		params["voxkube_node"] = node.Name
	}
	if globalNoop(cluster) {
		params["noop"] = true
	}

	classes := node.Spec.Classification.Classes
	if classes == nil {
		classes = []string{}
	}
	return yaml.Marshal(map[string]interface{}{
		"environment": env,
		"classes":     classes,
		"parameters":  params,
	})
}

// renderDefaultENC is returned for certnames without a VoxNode classification.
func renderDefaultENC(cluster *openvoxv1alpha1.VoxCluster) []byte {
	params := map[string]interface{}{}
	if injectMetadata(cluster) {
		params["voxkube_cluster"] = cluster.Name
		params["voxkube_namespace"] = cluster.Namespace
	}
	if globalNoop(cluster) {
		params["noop"] = true
	}
	out, _ := yaml.Marshal(map[string]interface{}{
		"environment": encDefaultEnvironment(cluster),
		"classes":     []string{},
		"parameters":  params,
	})
	return out
}

// renderENCScript renders the exec node terminus script for the configured ENC mode.
func renderENCScript(cluster *openvoxv1alpha1.VoxCluster) string {
	var b strings.Builder
	b.WriteString(`#!/bin/sh
# Rendered by voxkube-operator. Puppet calls this script with the certname as $1.
CERTNAME="$1"
F="/etc/puppetlabs/puppet/enc-nodes/${CERTNAME}.yaml"
if [ -f "$F" ]; then
  cat "$F"
  exit 0
fi
`)
	if encMode(cluster) == encModeExternalHTTP {
		if nc := cluster.Spec.Workflows.NodeClassification; nc != nil && nc.ExternalURL != "" {
			// 200 classifies the node, 404 means "not known there" and falls back to the default; anything else
			// (5xx, timeout, connection refused) fails the catalog request instead of silently compiling the default.
			fmt.Fprintf(&b, `BODY=$(mktemp) || exit 1
CODE=$(curl -sS --max-time 10 -o "$BODY" -w '%%{http_code}' %s"/${CERTNAME}") || CODE=000
case "$CODE" in
  200)
    cat "$BODY"
    rm -f "$BODY"
    exit 0
    ;;
  404)
    rm -f "$BODY"
    ;;
  *)
    echo "external ENC returned HTTP $CODE for ${CERTNAME}" >&2
    head -c 2000 "$BODY" >&2
    rm -f "$BODY"
    exit 1
    ;;
esac
`, shellSingleQuote(strings.TrimRight(nc.ExternalURL, "/")))
		}
	}
	b.WriteString("cat /etc/puppetlabs/puppet/enc-default.yaml\n")
	return b.String()
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func reportProcessors(cluster *openvoxv1alpha1.VoxCluster) (string, string) {
	var procs []string
	webhook := ""
	if rp := cluster.Spec.Workflows.Reports; rp != nil {
		procs = append(procs, rp.Processors...)
		webhook = rp.WebhookURL
	}
	if len(procs) == 0 {
		if databaseEnabled(cluster) {
			procs = []string{"puppetdb"}
		} else {
			procs = []string{"store"}
		}
	}
	if webhook != "" {
		has := false
		for _, p := range procs {
			if p == "http" {
				has = true
			}
		}
		if !has {
			procs = append(procs, "http")
		}
	}
	return strings.Join(procs, ","), webhook
}

// renderServerPuppetConf renders puppet.conf for the compiler pods.
func renderServerPuppetConf(cluster *openvoxv1alpha1.VoxCluster) string {
	var b strings.Builder
	fmt.Fprintf(&b, `[main]
certname = %s
confdir = /etc/puppetlabs/puppet
codedir = /etc/puppetlabs/code
vardir = /var/cache/puppetlabs/vardir
logdir = /var/log/puppetlabs/puppetserver
rundir = /var/run/puppetlabs/puppetserver
ssldir = /var/cache/puppetlabs/ssldir
hiera_config = /etc/puppetlabs/puppet/hiera.yaml

[server]
`, serviceCertname(cluster))
	reports, webhook := reportProcessors(cluster)
	fmt.Fprintf(&b, "reports = %s\n", reports)
	if webhook != "" {
		fmt.Fprintf(&b, "reporturl = %s\n", webhook)
	}
	if databaseEnabled(cluster) {
		b.WriteString("storeconfigs = true\nstoreconfigs_backend = puppetdb\n")
	}
	if encMode(cluster) != encModeNone {
		b.WriteString("node_terminus = exec\nexternal_nodes = /etc/puppetlabs/puppet/enc.sh\n")
	}
	return b.String()
}

const hieraYAML = `---
version: 5
defaults:
  datadir: /etc/puppetlabs/puppet/hiera-secrets
  data_hash: yaml_data
hierarchy:
  - name: "Per-node Kubernetes Secrets & Certificates"
    path: "nodes_%{trusted.certname}.yaml"
  - name: "Role-scoped Kubernetes Secrets"
    path: "roles_%{role}.yaml"
  - name: "Common Cluster Kubernetes Secrets"
    path: "common.yaml"
`

const routesYAML = `---
server:
  facts:
    terminus: puppetdb
    cache: yaml
`

func renderBootstrapServerScript(certname string) string {
	return fmt.Sprintf(`#!/bin/sh
set -e
SSLDIR=/var/cache/puppetlabs/ssldir
mkdir -p "$SSLDIR/certs" "$SSLDIR/private_keys" "$SSLDIR/public_keys" "$SSLDIR/certificate_requests" "$SSLDIR/private"
cp /etc/puppetlabs/puppet/ssl-certs/ca.crt "$SSLDIR/certs/ca.pem"
cp /etc/puppetlabs/puppet/ssl-certs/crl.pem "$SSLDIR/crl.pem"
cp /etc/puppetlabs/puppet/ssl-certs/tls.crt "$SSLDIR/certs/%[1]s.pem"
cp /etc/puppetlabs/puppet/ssl-certs/tls.key "$SSLDIR/private_keys/%[1]s.pem"
cp /etc/puppetlabs/puppet/ssl-certs/tls.pub "$SSLDIR/public_keys/%[1]s.pem"
chmod 0640 "$SSLDIR/private_keys/%[1]s.pem"

# Demo environment, only when no real code source is configured.
if [ "${VOXKUBE_SEED_DEMO:-false}" = "true" ]; then
ENVDIR=/etc/puppetlabs/code/environments/production
if [ ! -f "$ENVDIR/manifests/site.pp" ]; then
  mkdir -p "$ENVDIR/manifests" "$ENVDIR/modules/profile/manifests"
  cat <<'SITEPP' > "$ENVDIR/manifests/site.pp"
node default {
  $msg   = getvar('welcome_message') ? { undef => 'Managed by VoxKube Operator', default => getvar('welcome_message') }
  $clust = getvar('voxkube_cluster') ? { undef => 'production', default => getvar('voxkube_cluster') }
  $role  = getvar('role') ? { undef => 'default', default => getvar('role') }
  file { '/etc/voxkube-node.txt':
    ensure  => file,
    content => "Node: ${trusted['certname']}\nCluster: ${clust}\nRole: ${role}\nMessage: ${msg}\n",
    mode    => '0644',
  }
}
SITEPP
  cat <<'NGINXPP' > "$ENVDIR/modules/profile/manifests/nginx.pp"
class profile::nginx {
  $port = getvar('http_port') ? { undef => '80', default => getvar('http_port') }
  $msg  = getvar('welcome_message') ? { undef => 'Welcome to VoxKube', default => getvar('welcome_message') }
  file { '/etc/voxkube-nginx.conf':
    ensure  => file,
    content => "# Managed by VoxKube OpenVox Operator\nserver {\n  listen ${port};\n  server_name ${trusted['certname']};\n  # ${msg}\n}\n",
    mode    => '0644',
  }
}
NGINXPP
fi
fi
`, certname)
}

func (r *VoxClusterReconciler) reconcileHieraBridge(ctx context.Context, cluster *openvoxv1alpha1.VoxCluster) (string, error) {
	secName := fmt.Sprintf("%s-hiera-secrets", cluster.Name)
	if cluster.Spec.HieraSecrets.Enabled != nil && !*cluster.Spec.HieraSecrets.Enabled {
		return "", nil
	}

	nodeList := &openvoxv1alpha1.VoxNodeList{}
	if err := r.List(ctx, nodeList, client.InNamespace(cluster.Namespace)); err != nil {
		return "", err
	}

	bundle, err := RenderHieraSecretsBundle(ctx, r.Client, cluster, nodeList.Items)
	if err != nil {
		return "", err
	}

	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secName,
			Namespace: cluster.Namespace,
			Labels:    voxClusterLabels(cluster.Name, "hiera-secrets"),
		},
		Type: corev1.SecretTypeOpaque,
		Data: bundle,
	}
	if err := controllerutil.SetControllerReference(cluster, sec, r.Scheme); err != nil {
		return "", err
	}
	if err := r.createOrUpdateSecret(ctx, sec); err != nil {
		return "", err
	}
	return secName, nil
}

func (r *VoxClusterReconciler) reconcileENCConfigMap(ctx context.Context, cluster *openvoxv1alpha1.VoxCluster) error {
	nodeList := &openvoxv1alpha1.VoxNodeList{}
	if err := r.List(ctx, nodeList, client.InNamespace(cluster.Namespace)); err != nil {
		return err
	}

	data := make(map[string]string)
	for i := range nodeList.Items {
		n := &nodeList.Items[i]
		if !nodeServedBy(n, cluster) {
			continue
		}
		encYAML, err := RenderVoxNodeENCYAML(n, cluster)
		if err != nil {
			return err
		}
		data[n.Spec.Certname+".yaml"] = string(encYAML)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-enc-nodes", cluster.Name),
			Namespace: cluster.Namespace,
			Labels:    voxClusterLabels(cluster.Name, "enc"),
		},
		Data: data,
	}
	if err := controllerutil.SetControllerReference(cluster, cm, r.Scheme); err != nil {
		return err
	}
	return r.createOrUpdateConfigMap(ctx, cm)
}

func autoscalingEnabled(cluster *openvoxv1alpha1.VoxCluster) bool {
	return cluster.Spec.Server.Autoscaling != nil && cluster.Spec.Server.Autoscaling.Enabled
}

func (r *VoxClusterReconciler) reconcileServerDeployment(ctx context.Context, cluster *openvoxv1alpha1.VoxCluster, st *clusterState) error {
	deployName := fmt.Sprintf("%s-server", cluster.Name)
	plan := st.code
	img := serverImage(cluster)
	javaArgs := cluster.Spec.Server.JavaArgs
	if javaArgs == "" {
		javaArgs = "-Xms1g -Xmx1g"
	}
	maxActive := cluster.Spec.Server.MaxActiveInstances
	if maxActive == 0 {
		maxActive = 2
	}

	existing := &appsv1.Deployment{}
	existingFound := r.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: deployName}, existing) == nil

	replicas := int32(2)
	if cluster.Spec.Server.Replicas != nil {
		replicas = *cluster.Spec.Server.Replicas
	}
	if autoscalingEnabled(cluster) {
		// The HPA owns the replica count; keep whatever it chose.
		if existingFound && existing.Spec.Replicas != nil {
			replicas = *existing.Spec.Replicas
		} else if mr := cluster.Spec.Server.Autoscaling.MinReplicas; mr != nil {
			replicas = *mr
		}
	}

	certname := serviceCertname(cluster)
	webserverConf := `webserver: {
    access-log-config: /etc/puppetlabs/puppetserver/request-logging.xml
    client-auth: want
    ssl-host: 0.0.0.0
    ssl-port: 8140
    ssl-cert: /etc/puppetlabs/puppet/ssl-certs/tls.crt
    ssl-key: /etc/puppetlabs/puppet/ssl-certs/tls.key
    ssl-ca-cert: /etc/puppetlabs/puppet/ssl-certs/ca.crt
    ssl-crl-path: /etc/puppetlabs/puppet/ssl-certs/crl.pem
}
`
	caCfg := `puppetlabs.services.ca.certificate-authority-disabled-service/certificate-authority-disabled-service
puppetlabs.trapperkeeper.services.watcher.filesystem-watch-service/filesystem-watch-service
`
	puppetdbConf := fmt.Sprintf("[main]\nserver_urls = https://%s-db.%s.svc:8081\nsoft_write_failure = false\n", cluster.Name, cluster.Namespace)

	srvCMName := fmt.Sprintf("%s-server-config", cluster.Name)
	srvCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: srvCMName, Namespace: cluster.Namespace, Labels: voxClusterLabels(cluster.Name, "server")},
		Data: map[string]string{
			"webserver.conf":      webserverConf,
			"ca.cfg":              caCfg,
			"puppet.conf":         renderServerPuppetConf(cluster),
			"puppetserver.conf":   puppetserverConfig(maxActive),
			"puppetdb.conf":       puppetdbConf,
			"routes.yaml":         routesYAML,
			"enc.sh":              renderENCScript(cluster),
			"enc-default.yaml":    string(renderDefaultENC(cluster)),
			"hiera.yaml":          hieraYAML,
			"bootstrap-server.sh": renderBootstrapServerScript(certname),
		},
	}
	if err := controllerutil.SetControllerReference(cluster, srvCM, r.Scheme); err != nil {
		return err
	}
	if err := r.createOrUpdateConfigMap(ctx, srvCM); err != nil {
		return err
	}

	podAnnotations := map[string]string{
		AnnotationConfigHash: hashStrings(srvCM.Data),
		AnnotationTLSHash:    st.serverTLSHash,
	}
	for k, v := range plan.podAnnotations {
		if v != "" {
			podAnnotations[k] = v
		}
	}

	srvConfigMode := int32(0o755)
	codeVol := plan.codeVolume
	volumes := []corev1.Volume{
		// The compilers' own certificate, CA certificate and CRL: never the CA key (<cluster>-ca-tls).
		{Name: "tls", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: serverSecretName(cluster)}}},
		{Name: "server-config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: srvCMName},
			DefaultMode:          &srvConfigMode,
		}}},
		codeVol,
		{Name: "puppetserver-cache", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "puppetserver-log", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "puppetserver-run", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "enc-nodes", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: fmt.Sprintf("%s-enc-nodes", cluster.Name)},
		}}},
	}
	volumes = append(volumes, plan.volumes...)

	sub := func(file, path string) corev1.VolumeMount {
		return corev1.VolumeMount{Name: "server-config", MountPath: path, SubPath: file, ReadOnly: true}
	}
	volumeMounts := []corev1.VolumeMount{
		{Name: "tls", MountPath: "/etc/puppetlabs/puppet/ssl-certs", ReadOnly: true},
		sub("webserver.conf", "/etc/puppetlabs/puppetserver/conf.d/webserver.conf"),
		sub("puppetserver.conf", "/etc/puppetlabs/puppetserver/conf.d/puppetserver.conf"),
		sub("ca.cfg", "/etc/puppetlabs/puppetserver/services.d/ca.cfg"),
		sub("puppet.conf", "/etc/puppetlabs/puppet/puppet.conf"),
		sub("enc.sh", "/etc/puppetlabs/puppet/enc.sh"),
		sub("enc-default.yaml", "/etc/puppetlabs/puppet/enc-default.yaml"),
		sub("hiera.yaml", "/etc/puppetlabs/puppet/hiera.yaml"),
		{Name: "puppetserver-cache", MountPath: "/var/cache/puppetlabs"},
		{Name: "puppetserver-log", MountPath: "/var/log/puppetlabs"},
		{Name: "puppetserver-run", MountPath: "/var/run/puppetlabs"},
		{Name: codeVol.Name, MountPath: codeDir, ReadOnly: plan.codeReadOnly},
		{Name: "enc-nodes", MountPath: "/etc/puppetlabs/puppet/enc-nodes", ReadOnly: true},
	}
	if databaseEnabled(cluster) {
		volumeMounts = append(volumeMounts,
			sub("puppetdb.conf", "/etc/puppetlabs/puppet/puppetdb.conf"),
			sub("routes.yaml", "/etc/puppetlabs/puppet/routes.yaml"),
		)
	}
	if st.hieraSecretName != "" {
		volumes = append(volumes, corev1.Volume{Name: "hiera-secrets", VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: st.hieraSecretName},
		}})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{Name: "hiera-secrets", MountPath: "/etc/puppetlabs/puppet/hiera-secrets", ReadOnly: true})
	}

	initMounts := []corev1.VolumeMount{
		{Name: "tls", MountPath: "/etc/puppetlabs/puppet/ssl-certs", ReadOnly: true},
		sub("bootstrap-server.sh", "/etc/puppetlabs/puppet/bootstrap-server.sh"),
		{Name: "puppetserver-cache", MountPath: "/var/cache/puppetlabs"},
	}
	if plan.seedDemo {
		initMounts = append(initMounts, corev1.VolumeMount{Name: codeVol.Name, MountPath: codeDir})
	}
	initContainers := append([]corev1.Container{}, plan.initContainers...)
	initContainers = append(initContainers, corev1.Container{
		Name:            "server-init",
		Image:           img,
		ImagePullPolicy: cluster.Spec.Server.ImagePullPolicy,
		Command:         []string{"/bin/sh", "/etc/puppetlabs/puppet/bootstrap-server.sh"},
		Env:             []corev1.EnvVar{{Name: "VOXKUBE_SEED_DEMO", Value: fmt.Sprintf("%t", plan.seedDemo)}},
		SecurityContext: buildRestrictedContainerSecurityContext(),
		VolumeMounts:    initMounts,
	})

	containers := []corev1.Container{{
		Name:            "openvox-server",
		Image:           img,
		ImagePullPolicy: cluster.Spec.Server.ImagePullPolicy,
		SecurityContext: buildRestrictedContainerSecurityContext(),
		Env:             []corev1.EnvVar{{Name: "JAVA_ARGS", Value: javaArgs}},
		Ports:           []corev1.ContainerPort{{Name: "puppet", ContainerPort: 8140, Protocol: corev1.ProtocolTCP}},
		ReadinessProbe:  puppetStatusProbe(8140, corev1.URISchemeHTTPS),
		Resources:       cluster.Spec.Server.Resources,
		VolumeMounts:    volumeMounts,
	}}
	containers = append(containers, plan.sidecars...)

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: deployName, Namespace: cluster.Namespace, Labels: voxClusterLabels(cluster.Name, "server")},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: voxClusterLabels(cluster.Name, "server")},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: voxClusterLabels(cluster.Name, "server"), Annotations: podAnnotations},
				Spec: corev1.PodSpec{
					SecurityContext: buildOpenShiftPodSecurityContext(cluster.Spec.Server.SecurityContext),
					InitContainers:  initContainers,
					Containers:      containers,
					Volumes:         volumes,
				},
			},
		},
	}
	if err := controllerutil.SetControllerReference(cluster, deploy, r.Scheme); err != nil {
		return err
	}
	if err := r.createOrUpdateDeployment(ctx, deploy); err != nil {
		return err
	}

	st.desiredReplicas = replicas
	current := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: deployName}, current); err == nil {
		st.readyReplicas = current.Status.ReadyReplicas
		if current.Spec.Replicas != nil {
			st.desiredReplicas = *current.Spec.Replicas
		}
	}
	return nil
}

// reconcileAutoscaling creates, updates or removes the compiler HorizontalPodAutoscaler.
func (r *VoxClusterReconciler) reconcileAutoscaling(ctx context.Context, cluster *openvoxv1alpha1.VoxCluster) error {
	name := fmt.Sprintf("%s-server", cluster.Name)
	existing := &autoscalingv2.HorizontalPodAutoscaler{}
	err := r.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: name}, existing)
	found := err == nil
	if err != nil && !apierrors.IsNotFound(err) {
		if meta.IsNoMatchError(err) {
			return nil
		}
		return err
	}

	if !autoscalingEnabled(cluster) {
		if found && metav1.IsControlledBy(existing, cluster) {
			return client.IgnoreNotFound(r.Delete(ctx, existing))
		}
		return nil
	}

	as := cluster.Spec.Server.Autoscaling
	minReplicas := int32(2)
	if as.MinReplicas != nil {
		minReplicas = *as.MinReplicas
	}
	maxReplicas := as.MaxReplicas
	if maxReplicas < minReplicas {
		maxReplicas = minReplicas
	}
	target := int32(75)
	if as.TargetCPUUtilizationPercentage != nil {
		target = *as.TargetCPUUtilizationPercentage
	}

	desired := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cluster.Namespace, Labels: voxClusterLabels(cluster.Name, "server")},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: name},
			MinReplicas:    &minReplicas,
			MaxReplicas:    maxReplicas,
			Metrics: []autoscalingv2.MetricSpec{{
				Type: autoscalingv2.ResourceMetricSourceType,
				Resource: &autoscalingv2.ResourceMetricSource{
					Name:   corev1.ResourceCPU,
					Target: autoscalingv2.MetricTarget{Type: autoscalingv2.UtilizationMetricType, AverageUtilization: &target},
				},
			}},
		},
	}
	if err := controllerutil.SetControllerReference(cluster, desired, r.Scheme); err != nil {
		return err
	}
	h := specHash(desired.Spec)
	desired.Annotations = map[string]string{AnnotationSpecHash: h}
	if !found {
		return r.Create(ctx, desired)
	}
	// The API server defaults parts of the HPA spec (e.g. behavior), so compare what we rendered, not the live spec.
	if existing.Annotations[AnnotationSpecHash] == h && reflect.DeepEqual(existing.Labels, desired.Labels) {
		return nil
	}
	existing.Spec = desired.Spec
	existing.Labels = desired.Labels
	existing.Annotations = mergeStringMaps(existing.Annotations, desired.Annotations)
	return r.Update(ctx, existing)
}

func serviceTypeOrDefault(t corev1.ServiceType) corev1.ServiceType {
	if t == "" {
		return corev1.ServiceTypeClusterIP
	}
	return t
}

// reconcileNetworking creates the compiler Service and the Routes of both endpoints, and records their
// external URLs in st.
func (r *VoxClusterReconciler) reconcileNetworking(ctx context.Context, cluster *openvoxv1alpha1.VoxCluster, st *clusterState) error {
	ep := cluster.Spec.Networking.Server
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: cluster.Name, Namespace: cluster.Namespace, Labels: voxClusterLabels(cluster.Name, "server")},
		Spec: corev1.ServiceSpec{
			Type:     serviceTypeOrDefault(ep.ServiceType),
			Selector: voxClusterLabels(cluster.Name, "server"),
			Ports:    []corev1.ServicePort{{Name: "puppet", Port: 8140, TargetPort: intstr.FromInt32(8140), Protocol: corev1.ProtocolTCP}},
		},
	}
	if err := controllerutil.SetControllerReference(cluster, svc, r.Scheme); err != nil {
		return err
	}
	if err := r.createOrUpdateService(ctx, svc); err != nil {
		return err
	}
	if err := r.reconcileEndpointRoute(ctx, cluster, ep, cluster.Name, cluster.Name, "puppet"); err != nil {
		return err
	}
	st.serverURL = fmt.Sprintf("https://%s.%s.svc.cluster.local:8140", cluster.Name, cluster.Namespace)
	st.serverExternalURL = r.endpointExternalURL(ctx, cluster, ep, cluster.Name)

	caName := cluster.Name + "-ca"
	caEP := cluster.Spec.Networking.CA
	if !caEnabled(cluster) {
		caEP = openvoxv1alpha1.EndpointNetworkingSpec{} // removes a CA Route left over from when the CA was enabled
	}
	if err := r.reconcileEndpointRoute(ctx, cluster, caEP, caName, caName, "https-ca"); err != nil {
		return err
	}
	if caEnabled(cluster) {
		st.caExternalURL = r.endpointExternalURL(ctx, cluster, caEP, caName)
	}
	return nil
}

func routeHost(ep openvoxv1alpha1.EndpointNetworkingSpec) string {
	if ep.OpenShiftRoute != nil && ep.OpenShiftRoute.Host != "" {
		return ep.OpenShiftRoute.Host
	}
	return ep.ExternalHostname
}

// reconcileEndpointRoute creates or deletes the passthrough Route of one endpoint.
func (r *VoxClusterReconciler) reconcileEndpointRoute(
	ctx context.Context,
	cluster *openvoxv1alpha1.VoxCluster,
	ep openvoxv1alpha1.EndpointNetworkingSpec,
	routeName, serviceName, targetPort string,
) error {
	routeObj := &unstructured.Unstructured{}
	routeObj.SetGroupVersionKind(schema.GroupVersionKind{Group: "route.openshift.io", Version: "v1", Kind: "Route"})
	routeObj.SetName(routeName)
	routeObj.SetNamespace(cluster.Namespace)

	if ep.OpenShiftRoute == nil || !ep.OpenShiftRoute.Enabled {
		existing := &unstructured.Unstructured{}
		existing.SetGroupVersionKind(routeObj.GroupVersionKind())
		err := r.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: routeName}, existing)
		switch {
		case err == nil:
			if metav1.IsControlledBy(existing, cluster) {
				return client.IgnoreNotFound(r.Delete(ctx, existing))
			}
			return nil
		case apierrors.IsNotFound(err) || isNoMatch(err, "Route"):
			return nil
		default:
			return err
		}
	}

	routeObj.SetLabels(voxClusterLabels(cluster.Name, "route"))
	if len(ep.OpenShiftRoute.Annotations) > 0 {
		routeObj.SetAnnotations(ep.OpenShiftRoute.Annotations)
	}
	routeSpec := map[string]interface{}{
		"to":   map[string]interface{}{"kind": "Service", "name": serviceName},
		"port": map[string]interface{}{"targetPort": targetPort},
		"tls": map[string]interface{}{
			"termination":                   "passthrough",
			"insecureEdgeTerminationPolicy": "None",
		},
	}
	if host := routeHost(ep); host != "" {
		routeSpec["host"] = host
	}
	routeObj.Object["spec"] = routeSpec

	if err := controllerutil.SetControllerReference(cluster, routeObj, r.Scheme); err != nil {
		return err
	}
	if err := r.createOrUpdateUnstructured(ctx, routeObj); err != nil && !isNoMatch(err, "Route") {
		return err
	}
	return nil
}

// endpointExternalURL returns the URL agents outside the Kubernetes cluster should use, or "" when the endpoint
// has no external hostname. Routes listen on 443; LoadBalancer and ClusterIP (exposed by something else) on 8140;
// NodePort on the allocated node port.
func (r *VoxClusterReconciler) endpointExternalURL(ctx context.Context, cluster *openvoxv1alpha1.VoxCluster, ep openvoxv1alpha1.EndpointNetworkingSpec, serviceName string) string {
	if ep.OpenShiftRoute != nil && ep.OpenShiftRoute.Enabled {
		if host := routeHost(ep); host != "" {
			return fmt.Sprintf("https://%s:443", host)
		}
		return ""
	}
	if ep.ExternalHostname == "" {
		return ""
	}
	port := int32(8140)
	if serviceTypeOrDefault(ep.ServiceType) == corev1.ServiceTypeNodePort {
		svc := &corev1.Service{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: serviceName}, svc); err == nil &&
			len(svc.Spec.Ports) > 0 && svc.Spec.Ports[0].NodePort != 0 {
			port = svc.Spec.Ports[0].NodePort
		}
	}
	return fmt.Sprintf("https://%s:%d", ep.ExternalHostname, port)
}
