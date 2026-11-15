package controller

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	openvoxv1alpha1 "github.com/yboikov/voxkube-operator/api/v1alpha1"
)

const (
	// AnnotationSyncNow triggers an immediate outbound Git ls-remote check and g10k rollout.
	AnnotationSyncNow = "voxkube.io/sync-now"
	// AnnotationObservedCommit records the Git commit SHA injected into compiler Pod templates.
	AnnotationObservedCommit = "voxkube.io/observed-commit"
)

// VoxClusterReconciler reconciles a VoxCluster object.
type VoxClusterReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	Recorder       events.EventRecorder
	GitPoller      GitRemotePoller
	TokenExchanger *GitHubAppTokenExchanger
}

// +kubebuilder:rbac:groups=voxkube.io,resources=voxclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=voxkube.io,resources=voxclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=voxkube.io,resources=voxclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets;configmaps;services;persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=route.openshift.io,resources=routes,verbs=get;list;watch;create;update;patch;delete

// Reconcile converges a VoxCluster into a complete OpenVox stack (CA, Postgres, OpenVox DB, Hiera Secret Bridge, Compilers, Services, and OpenShift Route).
func (r *VoxClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	cluster := &openvoxv1alpha1.VoxCluster{}
	if err := r.Get(ctx, req.NamespacedName, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// 1. Reconcile CA PKI Secret (<cluster>-ca-tls)
	caSecretName, err := r.reconcileCAPKI(ctx, cluster)
	if err != nil {
		logger.Error(err, "failed to reconcile CA PKI")
		return ctrl.Result{}, err
	}

	// 2. Reconcile GitHub App token & Outbound Git Poller
	observedCommit, err := r.reconcileGitCodeSync(ctx, cluster)
	if err != nil {
		logger.Error(err, "failed to reconcile Git code sync")
		return ctrl.Result{}, err
	}

	// 3. Reconcile Built-in or External PostgreSQL & OpenVox DB (PuppetDB with TTL housekeeping)
	dbURL, err := r.reconcileDatabaseStack(ctx, cluster)
	if err != nil {
		logger.Error(err, "failed to reconcile OpenVox DB stack")
		return ctrl.Result{}, err
	}

	// 4. Reconcile ESO & cert-manager -> Hiera Secret Bridge
	hieraSecretName, err := r.reconcileHieraBridge(ctx, cluster)
	if err != nil {
		logger.Error(err, "failed to reconcile Hiera Secret Bridge")
		return ctrl.Result{}, err
	}

	// 5. Reconcile Kubernetes-native ENC ConfigMap
	if err := r.reconcileENCConfigMap(ctx, cluster); err != nil {
		logger.Error(err, "failed to reconcile ENC ConfigMap")
		return ctrl.Result{}, err
	}

	// 6. Reconcile OpenVox Server Compiler Deployment (OpenShift restricted-v2 SCC compliant + g10k)
	readyReplicas, err := r.reconcileServerDeployment(ctx, cluster, caSecretName, hieraSecretName, observedCommit)
	if err != nil {
		logger.Error(err, "failed to reconcile OpenVox Server Deployment")
		return ctrl.Result{}, err
	}

	// 7. Reconcile Server Service & optional OpenShift Route (route.openshift.io/v1)
	serverURL, err := r.reconcileNetworking(ctx, cluster)
	if err != nil {
		logger.Error(err, "failed to reconcile networking")
		return ctrl.Result{}, err
	}

	// 8. Update VoxCluster Status
	cluster.Status.Phase = openvoxv1alpha1.ClusterPhaseRunning
	cluster.Status.CASecretName = caSecretName
	cluster.Status.HieraSecretName = hieraSecretName
	cluster.Status.DatabaseURL = dbURL
	cluster.Status.ServerURL = serverURL
	cluster.Status.ObservedCommit = observedCommit
	cluster.Status.ReadyReplicas = readyReplicas

	meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cluster.Generation,
		Reason:             "StackReconciled",
		Message:            "VoxCluster CA, Database, Hiera Bridge, and Compilers reconciled",
	})

	if err := r.Status().Update(ctx, cluster); err != nil {
		return ctrl.Result{}, err
	}

	// If Git polling is configured, requeue after pollInterval
	if cluster.Spec.Code.Git != nil && cluster.Spec.Code.Git.PollInterval != "" {
		if d, err := time.ParseDuration(cluster.Spec.Code.Git.PollInterval); err == nil && d > 0 {
			return ctrl.Result{RequeueAfter: d}, nil
		}
	}

	return ctrl.Result{}, nil
}

func (r *VoxClusterReconciler) reconcileCAPKI(ctx context.Context, cluster *openvoxv1alpha1.VoxCluster) (string, error) {
	secName := fmt.Sprintf("%s-ca-tls", cluster.Name)
	existing := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: secName}, existing)
	if err == nil {
		return secName, nil
	}
	if !apierrors.IsNotFound(err) {
		return "", err
	}

	caCert, caKey, err := GenerateSelfSignedCA(fmt.Sprintf("VoxKube Puppet CA - %s", cluster.Name), 10*365*24*time.Hour)
	if err != nil {
		return "", err
	}

	dnsNames := []string{
		fmt.Sprintf("%s.%s.svc", cluster.Name, cluster.Namespace),
		fmt.Sprintf("%s.%s.svc.cluster.local", cluster.Name, cluster.Namespace),
	}
	if cluster.Spec.Networking.ExternalHostname != "" {
		dnsNames = append(dnsNames, cluster.Spec.Networking.ExternalHostname)
	}
	dnsNames = append(dnsNames, cluster.Spec.Networking.AdditionalDNSNames...)

	srvCert, srvKey, err := IssueClientCertificate(caCert, caKey, fmt.Sprintf("%s.%s.svc", cluster.Name, cluster.Namespace), dnsNames, 365*24*time.Hour)
	if err != nil {
		return "", err
	}

	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secName,
			Namespace: cluster.Namespace,
			Labels:    voxClusterLabels(cluster.Name, "ca"),
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			"ca.crt":  caCert,
			"ca.key":  caKey,
			"tls.crt": srvCert,
			"tls.key": srvKey,
		},
	}
	if err := controllerutil.SetControllerReference(cluster, sec, r.Scheme); err != nil {
		return "", err
	}
	if err := r.Create(ctx, sec); err != nil {
		return "", err
	}
	return secName, nil
}

func (r *VoxClusterReconciler) reconcileGitCodeSync(ctx context.Context, cluster *openvoxv1alpha1.VoxCluster) (string, error) {
	if cluster.Spec.Code.Git == nil {
		return "", nil
	}
	gitSpec := cluster.Spec.Code.Git
	var token string

	// Mint GitHub App installation token if configured
	if gitSpec.AuthType == "githubApp" && gitSpec.GitHubApp != nil {
		privSec := &corev1.Secret{}
		keySel := gitSpec.GitHubApp.PrivateKeySecretRef
		if err := r.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: keySel.Name}, privSec); err != nil {
			if !apierrors.IsNotFound(err) {
				return "", err
			}
		} else if pemBytes, ok := privSec.Data[keySel.Key]; ok && r.TokenExchanger != nil {
			mintedToken, _, err := r.TokenExchanger.ExchangeInstallationToken(
				ctx,
				gitSpec.GitHubApp.APIBaseURL,
				gitSpec.GitHubApp.AppID,
				gitSpec.GitHubApp.InstallationID,
				pemBytes,
			)
			if err != nil {
				return "", err
			}
			token = mintedToken

			// Store rotated g10k runtime token Secret for compiler pods
			tokenSecName := fmt.Sprintf("%s-g10k-git-token", cluster.Name)
			tokenSec := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tokenSecName,
					Namespace: cluster.Namespace,
					Labels:    voxClusterLabels(cluster.Name, "g10k-token"),
				},
				Type: corev1.SecretTypeOpaque,
				Data: map[string][]byte{
					"token": []byte(token),
				},
			}
			if err := controllerutil.SetControllerReference(cluster, tokenSec, r.Scheme); err != nil {
				return "", err
			}
			if err := r.createOrUpdateSecret(ctx, tokenSec); err != nil {
				return "", err
			}
		}
	}

	if r.GitPoller != nil {
		sha, err := r.GitPoller.ResolveHEAD(ctx, gitSpec.Remote, token)
		if err != nil {
			return cluster.Status.ObservedCommit, err
		}
		if syncTrigger := cluster.Annotations[AnnotationSyncNow]; syncTrigger != "" && sha == "" {
			return "sync-" + syncTrigger, nil
		}
		return sha, nil
	}

	if syncTrigger := cluster.Annotations[AnnotationSyncNow]; syncTrigger != "" {
		return "sync-" + syncTrigger, nil
	}
	return cluster.Status.ObservedCommit, nil
}

func (r *VoxClusterReconciler) reconcileDatabaseStack(ctx context.Context, cluster *openvoxv1alpha1.VoxCluster) (string, error) {
	if cluster.Spec.Database.Enabled != nil && !*cluster.Spec.Database.Enabled {
		return "", nil
	}

	pgHost := cluster.Spec.Database.Postgres.Host
	pgPort := cluster.Spec.Database.Postgres.Port
	if pgPort == 0 {
		pgPort = 5432
	}
	dbName := cluster.Spec.Database.Postgres.DatabaseName
	if dbName == "" {
		dbName = "openvoxdb"
	}

	// If mode is builtin (or default), reconcile built-in PostgreSQL StatefulSet and Service
	if cluster.Spec.Database.Postgres.Mode == "" || cluster.Spec.Database.Postgres.Mode == "builtin" {
		pgSvcName := fmt.Sprintf("%s-postgres", cluster.Name)
		pgHost = fmt.Sprintf("%s.%s.svc.cluster.local", pgSvcName, cluster.Namespace)

		pgImg := cluster.Spec.Database.Postgres.Image
		if pgImg == "" {
			pgImg = "quay.io/sclorg/postgresql-16-c9s:latest"
		}

		pgInitCMName := fmt.Sprintf("%s-postgres-init", cluster.Name)
		pgInitCM := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pgInitCMName,
				Namespace: cluster.Namespace,
				Labels:    voxClusterLabels(cluster.Name, "postgres"),
			},
			Data: map[string]string{
				"01-pg-trgm.sh": "psql -U postgres -d \"${POSTGRESQL_DATABASE}\" -c \"CREATE EXTENSION IF NOT EXISTS pg_trgm;\"\n",
			},
		}
		if err := controllerutil.SetControllerReference(cluster, pgInitCM, r.Scheme); err != nil {
			return "", err
		}
		if err := r.createOrUpdateConfigMap(ctx, pgInitCM); err != nil {
			return "", err
		}

		sts := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pgSvcName,
				Namespace: cluster.Namespace,
				Labels:    voxClusterLabels(cluster.Name, "postgres"),
			},
			Spec: appsv1.StatefulSetSpec{
				ServiceName: pgSvcName,
				Replicas:    new(int32(1)),
				Selector: &metav1.LabelSelector{
					MatchLabels: voxClusterLabels(cluster.Name, "postgres"),
				},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: voxClusterLabels(cluster.Name, "postgres"),
					},
					Spec: corev1.PodSpec{
						SecurityContext: buildOpenShiftPodSecurityContext(cluster.Spec.Server.SecurityContext),
						Containers: []corev1.Container{
							{
								Name:            "postgres",
								Image:           pgImg,
								SecurityContext: buildRestrictedContainerSecurityContext(),
								Env: []corev1.EnvVar{
									{Name: "POSTGRESQL_DATABASE", Value: dbName},
									{Name: "POSTGRESQL_USER", Value: "openvoxdb"},
									{Name: "POSTGRESQL_PASSWORD", Value: "openvoxdb"},
									{Name: "POSTGRES_DB", Value: dbName},
									{Name: "POSTGRES_USER", Value: "openvoxdb"},
									{Name: "POSTGRES_PASSWORD", Value: "openvoxdb"},
								},
								Ports: []corev1.ContainerPort{
									{Name: "postgres", ContainerPort: pgPort, Protocol: corev1.ProtocolTCP},
								},
								VolumeMounts: []corev1.VolumeMount{
									{Name: "pgdata", MountPath: "/var/lib/pgsql/data"},
									{Name: "pg-init", MountPath: "/opt/app-root/src/postgresql-start/01-pg-trgm.sh", SubPath: "01-pg-trgm.sh", ReadOnly: true},
								},
							},
						},
						Volumes: []corev1.Volume{
							{
								Name: "pgdata",
								VolumeSource: corev1.VolumeSource{
									EmptyDir: &corev1.EmptyDirVolumeSource{},
								},
							},
							{
								Name: "pg-init",
								VolumeSource: corev1.VolumeSource{
									ConfigMap: &corev1.ConfigMapVolumeSource{
										LocalObjectReference: corev1.LocalObjectReference{Name: pgInitCMName},
									},
								},
							},
						},
					},
				},
			},
		}
		if err := controllerutil.SetControllerReference(cluster, sts, r.Scheme); err != nil {
			return "", err
		}
		if err := r.createOrUpdateStatefulSet(ctx, sts); err != nil {
			return "", err
		}

		pgSvc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pgSvcName,
				Namespace: cluster.Namespace,
				Labels:    voxClusterLabels(cluster.Name, "postgres"),
			},
			Spec: corev1.ServiceSpec{
				Selector: voxClusterLabels(cluster.Name, "postgres"),
				Ports: []corev1.ServicePort{
					{Name: "postgres", Port: pgPort, TargetPort: intstr.FromInt32(pgPort), Protocol: corev1.ProtocolTCP},
				},
			},
		}
		if err := controllerutil.SetControllerReference(cluster, pgSvc, r.Scheme); err != nil {
			return "", err
		}
		if err := r.createOrUpdateService(ctx, pgSvc); err != nil {
			return "", err
		}
	}

	// Reconcile OpenVox DB (PuppetDB) ConfigMap with housekeeping TTLs (node-ttl, node-purge-ttl, report-ttl)
	hk := cluster.Spec.Database.Housekeeping
	nodeTTL := hk.NodeTTL
	if nodeTTL == "" {
		nodeTTL = "7d"
	}
	nodePurgeTTL := hk.NodePurgeTTL
	if nodePurgeTTL == "" {
		nodePurgeTTL = "14d"
	}
	reportTTL := hk.ReportTTL
	if reportTTL == "" {
		reportTTL = "14d"
	}
	gcInterval := hk.GCInterval
	if gcInterval == 0 {
		gcInterval = 60
	}

	databaseIni := fmt.Sprintf(`[database]
subname = //%s:%d/%s
username = openvoxdb
password = openvoxdb
migrator-username = openvoxdb
migrator-password = openvoxdb
node-ttl = %s
node-purge-ttl = %s
report-ttl = %s
gc-interval = %d
`, pgHost, pgPort, dbName, nodeTTL, nodePurgeTTL, reportTTL, gcInterval)

	jettyIni := `[jetty]
host = 0.0.0.0
port = 8080
ssl-host = 0.0.0.0
ssl-port = 8081
ssl-key = /etc/puppetlabs/puppetdb/ssl/tls.key
ssl-cert = /etc/puppetlabs/puppetdb/ssl/tls.crt
ssl-ca-cert = /etc/puppetlabs/puppetdb/ssl/ca.crt
access-log-config = /etc/puppetlabs/puppetdb/request-logging.xml
`

	dbCMName := fmt.Sprintf("%s-db-config", cluster.Name)
	dbCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dbCMName,
			Namespace: cluster.Namespace,
			Labels:    voxClusterLabels(cluster.Name, "db"),
		},
		Data: map[string]string{
			"database.ini": databaseIni,
			"jetty.ini":    jettyIni,
		},
	}
	if err := controllerutil.SetControllerReference(cluster, dbCM, r.Scheme); err != nil {
		return "", err
	}
	if err := r.createOrUpdateConfigMap(ctx, dbCM); err != nil {
		return "", err
	}

	// Reconcile OpenVox DB Deployment & Service
	dbDeployName := fmt.Sprintf("%s-db", cluster.Name)
	dbImg := cluster.Spec.Database.Image
	if dbImg == "" {
		dbImg = "ghcr.io/slauger/openvox-db:latest"
	}
	dbReplicas := int32(1)
	if cluster.Spec.Database.Replicas != nil {
		dbReplicas = *cluster.Spec.Database.Replicas
	}

	dbDeploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dbDeployName,
			Namespace: cluster.Namespace,
			Labels:    voxClusterLabels(cluster.Name, "db"),
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &dbReplicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: voxClusterLabels(cluster.Name, "db"),
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: voxClusterLabels(cluster.Name, "db"),
				},
				Spec: corev1.PodSpec{
					SecurityContext: buildOpenShiftPodSecurityContext(cluster.Spec.Server.SecurityContext),
					Containers: []corev1.Container{
						{
							Name:            "openvox-db",
							Image:           dbImg,
							SecurityContext: buildRestrictedContainerSecurityContext(),
							Ports: []corev1.ContainerPort{
								{Name: "http", ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
								{Name: "https", ContainerPort: 8081, Protocol: corev1.ProtocolTCP},
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "db-config", MountPath: "/etc/puppetlabs/puppetdb/conf.d/database.ini", SubPath: "database.ini", ReadOnly: true},
								{Name: "db-config", MountPath: "/etc/puppetlabs/puppetdb/conf.d/jetty.ini", SubPath: "jetty.ini", ReadOnly: true},
								{Name: "ca-tls", MountPath: "/etc/puppetlabs/puppetdb/ssl", ReadOnly: true},
								{Name: "db-vardir", MountPath: "/opt/puppetlabs/server/data/puppetdb"},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "db-config",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{Name: dbCMName},
								},
							},
						},
						{
							Name: "ca-tls",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{SecretName: fmt.Sprintf("%s-ca-tls", cluster.Name)},
							},
						},
						{
							Name: "db-vardir",
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{},
							},
						},
					},
				},
			},
		},
	}
	if err := controllerutil.SetControllerReference(cluster, dbDeploy, r.Scheme); err != nil {
		return "", err
	}
	if err := r.createOrUpdateDeployment(ctx, dbDeploy); err != nil {
		return "", err
	}

	dbSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dbDeployName,
			Namespace: cluster.Namespace,
			Labels:    voxClusterLabels(cluster.Name, "db"),
		},
		Spec: corev1.ServiceSpec{
			Selector: voxClusterLabels(cluster.Name, "db"),
			Ports: []corev1.ServicePort{
				{Name: "http", Port: 8080, TargetPort: intstr.FromInt32(8080), Protocol: corev1.ProtocolTCP},
				{Name: "https", Port: 8081, TargetPort: intstr.FromInt32(8081), Protocol: corev1.ProtocolTCP},
			},
		},
	}
	if err := controllerutil.SetControllerReference(cluster, dbSvc, r.Scheme); err != nil {
		return "", err
	}
	if err := r.createOrUpdateService(ctx, dbSvc); err != nil {
		return "", err
	}

	return fmt.Sprintf("http://%s.%s.svc.cluster.local:8080", dbDeployName, cluster.Namespace), nil
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
		if n.Spec.ClusterRef != cluster.Name || n.Spec.State == openvoxv1alpha1.NodeStateDecommissioned {
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

func (r *VoxClusterReconciler) reconcileServerDeployment(
	ctx context.Context,
	cluster *openvoxv1alpha1.VoxCluster,
	caSecretName, hieraSecretName, observedCommit string,
) (int32, error) {
	deployName := fmt.Sprintf("%s-server", cluster.Name)
	replicas := int32(2)
	if cluster.Spec.Server.Replicas != nil {
		replicas = *cluster.Spec.Server.Replicas
	}
	img := cluster.Spec.Server.Image
	if img == "" {
		img = "ghcr.io/slauger/openvox-server:latest"
	}
	javaArgs := cluster.Spec.Server.JavaArgs
	if javaArgs == "" {
		javaArgs = "-Xms1g -Xmx1g"
	}

	// Reconcile Server ConfigMap (webserver.conf, ca.cfg, puppet.conf, puppetserver.conf) so JRuby/Jetty uses mounted Secret certs and writable subdirectory paths
	srvCMName := fmt.Sprintf("%s-server-config", cluster.Name)
	webserverConf := `webserver: {
    access-log-config: /etc/puppetlabs/puppetserver/request-logging.xml
    client-auth: want
    ssl-host: 0.0.0.0
    ssl-port: 8140
    ssl-cert: /etc/puppetlabs/puppet/ssl-certs/tls.crt
    ssl-key: /etc/puppetlabs/puppet/ssl-certs/tls.key
    ssl-ca-cert: /etc/puppetlabs/puppet/ssl-certs/ca.crt
}
`
	caCfg := `puppetlabs.services.ca.certificate-authority-disabled-service/certificate-authority-disabled-service
puppetlabs.trapperkeeper.services.watcher.filesystem-watch-service/filesystem-watch-service
`
	puppetConf := `[main]
confdir = /etc/puppetlabs/puppet
codedir = /etc/puppetlabs/code
vardir = /var/cache/puppetlabs/vardir
logdir = /var/log/puppetlabs/puppetserver
rundir = /var/run/puppetlabs/puppetserver
ssldir = /var/cache/puppetlabs/ssldir
`
	maxActive := cluster.Spec.Server.MaxActiveInstances
	if maxActive == 0 {
		maxActive = 1
	}
	puppetserverConf := fmt.Sprintf(`jruby-puppet: {
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

	srvCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      srvCMName,
			Namespace: cluster.Namespace,
			Labels:    voxClusterLabels(cluster.Name, "server"),
		},
		Data: map[string]string{
			"webserver.conf":    webserverConf,
			"ca.cfg":            caCfg,
			"puppet.conf":       puppetConf,
			"puppetserver.conf": puppetserverConf,
		},
	}
	if err := controllerutil.SetControllerReference(cluster, srvCM, r.Scheme); err != nil {
		return 0, err
	}
	if err := r.createOrUpdateConfigMap(ctx, srvCM); err != nil {
		return 0, err
	}

	podAnnotations := make(map[string]string)
	if observedCommit != "" {
		podAnnotations[AnnotationObservedCommit] = observedCommit
	}
	if cluster.Spec.Workflows.Maintenance != nil && cluster.Spec.Workflows.Maintenance.GlobalNoop {
		podAnnotations["voxkube.io/global-noop"] = "true"
	}

	volumes := []corev1.Volume{
		{
			Name: "ca-tls",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: caSecretName},
			},
		},
		{
			Name: "server-config",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: srvCMName},
				},
			},
		},
		{
			Name: "puppet-code",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
		{
			Name: "puppetserver-cache",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
		{
			Name: "puppetserver-log",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
		{
			Name: "puppetserver-run",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
		{
			Name: "enc-nodes",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: fmt.Sprintf("%s-enc-nodes", cluster.Name)},
				},
			},
		},
	}

	volumeMounts := []corev1.VolumeMount{
		{Name: "ca-tls", MountPath: "/etc/puppetlabs/puppet/ssl-certs", ReadOnly: true},
		{Name: "server-config", MountPath: "/etc/puppetlabs/puppetserver/conf.d/webserver.conf", SubPath: "webserver.conf", ReadOnly: true},
		{Name: "server-config", MountPath: "/etc/puppetlabs/puppetserver/conf.d/puppetserver.conf", SubPath: "puppetserver.conf", ReadOnly: true},
		{Name: "server-config", MountPath: "/etc/puppetlabs/puppetserver/services.d/ca.cfg", SubPath: "ca.cfg", ReadOnly: true},
		{Name: "server-config", MountPath: "/etc/puppetlabs/puppet/puppet.conf", SubPath: "puppet.conf", ReadOnly: true},
		{Name: "puppetserver-cache", MountPath: "/var/cache/puppetlabs"},
		{Name: "puppetserver-log", MountPath: "/var/log/puppetlabs"},
		{Name: "puppetserver-run", MountPath: "/var/run/puppetlabs"},
		{Name: "puppet-code", MountPath: "/etc/puppetlabs/code"},
		{Name: "enc-nodes", MountPath: "/etc/puppetlabs/puppet/enc-nodes", ReadOnly: true},
	}

	if hieraSecretName != "" {
		volumes = append(volumes, corev1.Volume{
			Name: "hiera-secrets",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: hieraSecretName},
			},
		})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      "hiera-secrets",
			MountPath: "/etc/puppetlabs/puppet/hiera-secrets",
			ReadOnly:  true,
		})
	}

	var initContainers []corev1.Container
	containers := []corev1.Container{
		{
			Name:            "openvox-server",
			Image:           img,
			ImagePullPolicy: cluster.Spec.Server.ImagePullPolicy,
			SecurityContext: buildRestrictedContainerSecurityContext(),
			Env: []corev1.EnvVar{
				{Name: "JAVA_ARGS", Value: javaArgs},
				{Name: "VOXKUBE_OBSERVED_COMMIT", Value: observedCommit},
			},
			Ports: []corev1.ContainerPort{
				{Name: "puppet", ContainerPort: 8140, Protocol: corev1.ProtocolTCP},
			},
			Resources:    cluster.Spec.Server.Resources,
			VolumeMounts: volumeMounts,
		},
	}

	// If Git/g10k mode is enabled, add g10k initial sync initContainer and g10k sidecar
	if cluster.Spec.Code.Mode == "" || cluster.Spec.Code.Mode == "git" {
		g10kArgs := []string{"-config", "/etc/puppetlabs/g10k/g10k.yaml"}
		if cluster.Spec.Code.Git != nil && (cluster.Spec.Code.Git.GenerateTypes == nil || *cluster.Spec.Code.Git.GenerateTypes) {
			g10kArgs = append(g10kArgs, "-generate-types")
		}
		initContainers = append(initContainers, corev1.Container{
			Name:            "g10k-init",
			Image:           img,
			Command:         []string{"/usr/local/bin/g10k"},
			Args:            g10kArgs,
			SecurityContext: buildRestrictedContainerSecurityContext(),
			VolumeMounts: []corev1.VolumeMount{
				{Name: "puppet-code", MountPath: "/etc/puppetlabs/code"},
			},
		})
		containers = append(containers, corev1.Container{
			Name:            "g10k-sync",
			Image:           img,
			Command:         []string{"/usr/local/bin/g10k-sidecar"},
			Args:            g10kArgs,
			SecurityContext: buildRestrictedContainerSecurityContext(),
			VolumeMounts: []corev1.VolumeMount{
				{Name: "puppet-code", MountPath: "/etc/puppetlabs/code"},
			},
		})
	}

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deployName,
			Namespace: cluster.Namespace,
			Labels:    voxClusterLabels(cluster.Name, "server"),
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: voxClusterLabels(cluster.Name, "server"),
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      voxClusterLabels(cluster.Name, "server"),
					Annotations: podAnnotations,
				},
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
		return 0, err
	}
	if err := r.createOrUpdateDeployment(ctx, deploy); err != nil {
		return 0, err
	}

	current := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: deployName}, current); err == nil {
		return current.Status.ReadyReplicas, nil
	}
	return 0, nil
}

func (r *VoxClusterReconciler) reconcileNetworking(ctx context.Context, cluster *openvoxv1alpha1.VoxCluster) (string, error) {
	svcType := cluster.Spec.Networking.ServiceType
	if svcType == "" {
		svcType = corev1.ServiceTypeClusterIP
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cluster.Name,
			Namespace: cluster.Namespace,
			Labels:    voxClusterLabels(cluster.Name, "server"),
		},
		Spec: corev1.ServiceSpec{
			Type:     svcType,
			Selector: voxClusterLabels(cluster.Name, "server"),
			Ports: []corev1.ServicePort{
				{Name: "puppet", Port: 8140, TargetPort: intstr.FromInt32(8140), Protocol: corev1.ProtocolTCP},
			},
		},
	}
	if err := controllerutil.SetControllerReference(cluster, svc, r.Scheme); err != nil {
		return "", err
	}
	if err := r.createOrUpdateService(ctx, svc); err != nil {
		return "", err
	}

	// Reconcile OpenShift Route (route.openshift.io/v1) with TLS passthrough if enabled
	if cluster.Spec.Networking.OpenShiftRoute != nil && cluster.Spec.Networking.OpenShiftRoute.Enabled {
		host := cluster.Spec.Networking.OpenShiftRoute.Host
		if host == "" {
			host = cluster.Spec.Networking.ExternalHostname
		}

		routeObj := &unstructured.Unstructured{}
		routeObj.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "route.openshift.io",
			Version: "v1",
			Kind:    "Route",
		})
		routeObj.SetName(cluster.Name)
		routeObj.SetNamespace(cluster.Namespace)
		routeObj.SetLabels(voxClusterLabels(cluster.Name, "route"))
		if len(cluster.Spec.Networking.OpenShiftRoute.Annotations) > 0 {
			routeObj.SetAnnotations(cluster.Spec.Networking.OpenShiftRoute.Annotations)
		}

		routeSpec := map[string]interface{}{
			"to": map[string]interface{}{
				"kind": "Service",
				"name": cluster.Name,
			},
			"port": map[string]interface{}{
				"targetPort": "puppet",
			},
			"tls": map[string]interface{}{
				"termination":                   "passthrough",
				"insecureEdgeTerminationPolicy": "None",
			},
		}
		if host != "" {
			routeSpec["host"] = host
		}
		routeObj.Object["spec"] = routeSpec

		if err := controllerutil.SetControllerReference(cluster, routeObj, r.Scheme); err != nil {
			return "", err
		}
		if err := r.createOrUpdateUnstructured(ctx, routeObj); err != nil {
			// Ignore NoMatchError if running on vanilla Kubernetes without OpenShift Route CRD installed
			if !meta.IsNoMatchError(err) && !strings.Contains(err.Error(), "no matches for kind \"Route\"") {
				return "", err
			}
		}
	}

	return fmt.Sprintf("https://%s.%s.svc.cluster.local:8140", cluster.Name, cluster.Namespace), nil
}

func voxClusterLabels(clusterName, component string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "voxkube",
		"app.kubernetes.io/instance":   clusterName,
		"app.kubernetes.io/component":  component,
		"app.kubernetes.io/managed-by": "voxkube-operator",
	}
}

func (r *VoxClusterReconciler) createOrUpdateSecret(ctx context.Context, desired *corev1.Secret) error {
	existing := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(existing.Data, desired.Data) || !reflect.DeepEqual(existing.Labels, desired.Labels) {
		existing.Data = desired.Data
		existing.Labels = desired.Labels
		return r.Update(ctx, existing)
	}
	return nil
}

func (r *VoxClusterReconciler) createOrUpdateConfigMap(ctx context.Context, desired *corev1.ConfigMap) error {
	existing := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(existing.Data, desired.Data) || !reflect.DeepEqual(existing.Labels, desired.Labels) {
		existing.Data = desired.Data
		existing.Labels = desired.Labels
		return r.Update(ctx, existing)
	}
	return nil
}

func (r *VoxClusterReconciler) createOrUpdateDeployment(ctx context.Context, desired *appsv1.Deployment) error {
	existing := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	existing.Spec = desired.Spec
	existing.Labels = desired.Labels
	return r.Update(ctx, existing)
}

func (r *VoxClusterReconciler) createOrUpdateStatefulSet(ctx context.Context, desired *appsv1.StatefulSet) error {
	existing := &appsv1.StatefulSet{}
	err := r.Get(ctx, types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	existing.Spec = desired.Spec
	existing.Labels = desired.Labels
	return r.Update(ctx, existing)
}

func (r *VoxClusterReconciler) createOrUpdateService(ctx context.Context, desired *corev1.Service) error {
	existing := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	desired.Spec.ClusterIP = existing.Spec.ClusterIP
	existing.Spec = desired.Spec
	existing.Labels = desired.Labels
	return r.Update(ctx, existing)
}

func (r *VoxClusterReconciler) createOrUpdateUnstructured(ctx context.Context, desired *unstructured.Unstructured) error {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(desired.GroupVersionKind())
	err := r.Get(ctx, types.NamespacedName{Namespace: desired.GetNamespace(), Name: desired.GetName()}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	desired.SetResourceVersion(existing.GetResourceVersion())
	return r.Update(ctx, desired)
}

// SetupWithManager registers VoxClusterReconciler with the controller manager.
func (r *VoxClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Ensure resource quantity import is used if needed
	_ = resource.MustParse("1Gi")
	return ctrl.NewControllerManagedBy(mgr).
		For(&openvoxv1alpha1.VoxCluster{}).
		Owns(&appsv1.Deployment{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Secret{}).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.findClustersForSecret),
		).
		Complete(r)
}

func (r *VoxClusterReconciler) findClustersForSecret(ctx context.Context, obj client.Object) []reconcile.Request {
	clusters := &openvoxv1alpha1.VoxClusterList{}
	if err := r.List(ctx, clusters, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, c := range clusters.Items {
		reqs = append(reqs, reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: c.Namespace, Name: c.Name},
		})
	}
	return reqs
}
