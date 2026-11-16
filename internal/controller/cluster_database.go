package controller

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	openvoxv1alpha1 "github.com/yboikov/voxkube-operator/api/v1alpha1"
)

func databaseEnabled(cluster *openvoxv1alpha1.VoxCluster) bool {
	return cluster.Spec.Database.Enabled == nil || *cluster.Spec.Database.Enabled
}

// dbServiceURL is the mTLS endpoint the operator and the compilers use to talk to OpenVox DB.
func dbServiceURL(cluster *openvoxv1alpha1.VoxCluster) string {
	return fmt.Sprintf("https://%s-db.%s.svc.cluster.local:8081", cluster.Name, cluster.Namespace)
}

func randomPassword() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// postgresCredentials returns the Secret name holding username/password and the values themselves.
func (r *VoxClusterReconciler) postgresCredentials(ctx context.Context, cluster *openvoxv1alpha1.VoxCluster, builtin bool) (string, string, string, error) {
	pg := cluster.Spec.Database.Postgres
	if pg.CredentialsSecretRef != "" {
		sec := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: pg.CredentialsSecretRef}, sec); err != nil {
			return "", "", "", fmt.Errorf("reading spec.database.postgres.credentialsSecretRef %q: %w", pg.CredentialsSecretRef, err)
		}
		user, pass := string(sec.Data["username"]), string(sec.Data["password"])
		if user == "" || pass == "" {
			return "", "", "", fmt.Errorf("secret %q must contain non-empty \"username\" and \"password\" keys", pg.CredentialsSecretRef)
		}
		return pg.CredentialsSecretRef, user, pass, nil
	}
	if !builtin {
		return "", "", "", fmt.Errorf("spec.database.postgres.credentialsSecretRef is required when mode is \"external\"")
	}

	// Built-in (dev/demo) PostgreSQL: generate credentials once and keep them.
	name := fmt.Sprintf("%s-postgres-credentials", cluster.Name)
	sec := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: name}, sec)
	if err == nil && len(sec.Data["username"]) > 0 && len(sec.Data["password"]) > 0 {
		return name, string(sec.Data["username"]), string(sec.Data["password"]), nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return "", "", "", err
	}
	pass, err := randomPassword()
	if err != nil {
		return "", "", "", err
	}
	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cluster.Namespace, Labels: voxClusterLabels(cluster.Name, "postgres")},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"username": []byte("openvoxdb"), "password": []byte(pass)},
	}
	if err := controllerutil.SetControllerReference(cluster, desired, r.Scheme); err != nil {
		return "", "", "", err
	}
	if err := r.createOrUpdateSecret(ctx, desired); err != nil {
		return "", "", "", err
	}
	return name, "openvoxdb", pass, nil
}

// reconcileDatabaseStack renders PostgreSQL (builtin only) and OpenVox DB.
func (r *VoxClusterReconciler) reconcileDatabaseStack(ctx context.Context, cluster *openvoxv1alpha1.VoxCluster, st *clusterState) error {
	st.dbEnabled = databaseEnabled(cluster)
	if !st.dbEnabled {
		return nil
	}

	pg := cluster.Spec.Database.Postgres
	builtin := pg.Mode == "" || pg.Mode == "builtin"
	pgPort := pg.Port
	if pgPort == 0 {
		pgPort = 5432
	}
	dbName := pg.DatabaseName
	if dbName == "" {
		dbName = "openvoxdb"
	}

	credSecret, pgUser, pgPass, err := r.postgresCredentials(ctx, cluster, builtin)
	if err != nil {
		return err
	}

	pgHost := pg.Host
	postgresReady := true
	if builtin {
		pgHost, postgresReady, err = r.reconcileBuiltinPostgres(ctx, cluster, credSecret, dbName, pgPort)
		if err != nil {
			return err
		}
	} else if pgHost == "" {
		return fmt.Errorf("spec.database.postgres.host is required when mode is \"external\"")
	}

	subname := fmt.Sprintf("//%s:%d/%s", pgHost, pgPort, url.PathEscape(dbName))
	if !builtin {
		sslMode := pg.SSLMode
		if sslMode == "" {
			sslMode = "require"
		}
		subname += "?sslmode=" + url.QueryEscape(sslMode)
	}

	if strings.ContainsAny(pgUser+pgPass, "\n\r") {
		return fmt.Errorf("the PostgreSQL username and password must not contain newlines")
	}

	hk := cluster.Spec.Database.Housekeeping
	gcInterval := hk.GCInterval
	if gcInterval <= 0 {
		gcInterval = 60
	}
	databaseIni := fmt.Sprintf(`[database]
subname = %s
username = %s
password = %s
migrator-username = %s
migrator-password = %s
node-ttl = %s
node-purge-ttl = %s
report-ttl = %s
gc-interval = %d
`, subname, pgUser, pgPass, pgUser, pgPass,
		firstNonEmpty(hk.NodeTTL, "7d"), firstNonEmpty(hk.NodePurgeTTL, "14d"), firstNonEmpty(hk.ReportTTL, "14d"),
		gcInterval)

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
		ObjectMeta: metav1.ObjectMeta{Name: dbCMName, Namespace: cluster.Namespace, Labels: voxClusterLabels(cluster.Name, "db")},
		Data:       map[string]string{"jetty.ini": jettyIni},
	}
	if err := controllerutil.SetControllerReference(cluster, dbCM, r.Scheme); err != nil {
		return err
	}
	if err := r.createOrUpdateConfigMap(ctx, dbCM); err != nil {
		return err
	}

	dbSecName := fmt.Sprintf("%s-db-database", cluster.Name)
	dbSec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: dbSecName, Namespace: cluster.Namespace, Labels: voxClusterLabels(cluster.Name, "db")},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"database.ini": []byte(databaseIni)},
	}
	if err := controllerutil.SetControllerReference(cluster, dbSec, r.Scheme); err != nil {
		return err
	}
	if err := r.createOrUpdateSecret(ctx, dbSec); err != nil {
		return err
	}

	dbDeployName := fmt.Sprintf("%s-db", cluster.Name)
	dbImg := cluster.Spec.Database.Image
	if dbImg == "" {
		dbImg = "ghcr.io/yboikov/openvox-db:latest"
	}
	dbReplicas := int32(1)
	if cluster.Spec.Database.Replicas != nil {
		dbReplicas = *cluster.Spec.Database.Replicas
	}

	dbDeploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: dbDeployName, Namespace: cluster.Namespace, Labels: voxClusterLabels(cluster.Name, "db")},
		Spec: appsv1.DeploymentSpec{
			Replicas: &dbReplicas,
			Selector: &metav1.LabelSelector{MatchLabels: voxClusterLabels(cluster.Name, "db")},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: voxClusterLabels(cluster.Name, "db"),
					Annotations: map[string]string{
						AnnotationConfigHash: hashStrings(map[string]string{"jetty.ini": jettyIni, "database.ini": databaseIni}),
						AnnotationTLSHash:    st.tlsHash,
					},
				},
				Spec: corev1.PodSpec{
					SecurityContext: buildOpenShiftPodSecurityContext(cluster.Spec.Server.SecurityContext),
					Containers: []corev1.Container{{
						Name:            "openvox-db",
						Image:           dbImg,
						SecurityContext: buildRestrictedContainerSecurityContext(),
						Ports: []corev1.ContainerPort{
							{Name: "http", ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
							{Name: "https", ContainerPort: 8081, Protocol: corev1.ProtocolTCP},
						},
						ReadinessProbe: puppetStatusProbe(8080, corev1.URISchemeHTTP),
						VolumeMounts: []corev1.VolumeMount{
							{Name: "db-database", MountPath: "/etc/puppetlabs/puppetdb/conf.d/database.ini", SubPath: "database.ini", ReadOnly: true},
							{Name: "db-config", MountPath: "/etc/puppetlabs/puppetdb/conf.d/jetty.ini", SubPath: "jetty.ini", ReadOnly: true},
							{Name: "ca-tls", MountPath: "/etc/puppetlabs/puppetdb/ssl", ReadOnly: true},
							{Name: "db-vardir", MountPath: "/opt/puppetlabs/server/data/puppetdb"},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "db-config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: dbCMName}}}},
						{Name: "db-database", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: dbSecName}}},
						{Name: "ca-tls", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: st.caSecretName}}},
						{Name: "db-vardir", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					},
				},
			},
		},
	}
	if err := controllerutil.SetControllerReference(cluster, dbDeploy, r.Scheme); err != nil {
		return err
	}
	if err := r.createOrUpdateDeployment(ctx, dbDeploy); err != nil {
		return err
	}

	dbSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: dbDeployName, Namespace: cluster.Namespace, Labels: voxClusterLabels(cluster.Name, "db")},
		Spec: corev1.ServiceSpec{
			Selector: voxClusterLabels(cluster.Name, "db"),
			Ports: []corev1.ServicePort{
				{Name: "http", Port: 8080, TargetPort: intstr.FromInt32(8080), Protocol: corev1.ProtocolTCP},
				{Name: "https", Port: 8081, TargetPort: intstr.FromInt32(8081), Protocol: corev1.ProtocolTCP},
			},
		},
	}
	if err := controllerutil.SetControllerReference(cluster, dbSvc, r.Scheme); err != nil {
		return err
	}
	if err := r.createOrUpdateService(ctx, dbSvc); err != nil {
		return err
	}

	st.dbURL = dbServiceURL(cluster)
	current := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: dbDeployName}, current); err == nil {
		st.dbReady = current.Status.ReadyReplicas > 0 && postgresReady
	}
	if !postgresReady {
		st.dbMessage = "Built-in PostgreSQL is not ready yet"
	}
	return nil
}

// reconcileBuiltinPostgres runs the dev/demo PostgreSQL StatefulSet on ephemeral storage.
func (r *VoxClusterReconciler) reconcileBuiltinPostgres(ctx context.Context, cluster *openvoxv1alpha1.VoxCluster, credSecret, dbName string, pgPort int32) (string, bool, error) {
	pg := cluster.Spec.Database.Postgres
	pgSvcName := fmt.Sprintf("%s-postgres", cluster.Name)
	pgImg := pg.Image
	if pgImg == "" {
		pgImg = "quay.io/sclorg/postgresql-16-c9s:latest"
	}

	pgInitCMName := fmt.Sprintf("%s-postgres-init", cluster.Name)
	pgInitCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: pgInitCMName, Namespace: cluster.Namespace, Labels: voxClusterLabels(cluster.Name, "postgres")},
		Data: map[string]string{
			"01-pg-trgm.sh": "psql -U postgres -d \"${POSTGRESQL_DATABASE}\" -c \"CREATE EXTENSION IF NOT EXISTS pg_trgm;\"\n",
		},
	}
	if err := controllerutil.SetControllerReference(cluster, pgInitCM, r.Scheme); err != nil {
		return "", false, err
	}
	if err := r.createOrUpdateConfigMap(ctx, pgInitCM); err != nil {
		return "", false, err
	}

	fromSecret := func(key string) *corev1.EnvVarSource {
		return &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: credSecret},
			Key:                  key,
		}}
	}

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: pgSvcName, Namespace: cluster.Namespace, Labels: voxClusterLabels(cluster.Name, "postgres")},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: pgSvcName,
			Replicas:    new(int32(1)),
			Selector:    &metav1.LabelSelector{MatchLabels: voxClusterLabels(cluster.Name, "postgres")},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: voxClusterLabels(cluster.Name, "postgres")},
				Spec: corev1.PodSpec{
					SecurityContext: buildOpenShiftPodSecurityContext(cluster.Spec.Server.SecurityContext),
					Containers: []corev1.Container{{
						Name:            "postgres",
						Image:           pgImg,
						SecurityContext: buildRestrictedContainerSecurityContext(),
						Env: []corev1.EnvVar{
							{Name: "POSTGRESQL_DATABASE", Value: dbName},
							{Name: "POSTGRESQL_USER", ValueFrom: fromSecret("username")},
							{Name: "POSTGRESQL_PASSWORD", ValueFrom: fromSecret("password")},
						},
						Ports: []corev1.ContainerPort{{Name: "postgres", ContainerPort: pgPort, Protocol: corev1.ProtocolTCP}},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(pgPort)},
							},
							InitialDelaySeconds: 5,
							PeriodSeconds:       10,
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "pgdata", MountPath: "/var/lib/pgsql/data"},
							{Name: "pg-init", MountPath: "/opt/app-root/src/postgresql-start/01-pg-trgm.sh", SubPath: "01-pg-trgm.sh", ReadOnly: true},
						},
					}},
					Volumes: []corev1.Volume{
						// Ephemeral on purpose: the built-in database is for development and demos only.
						{Name: "pgdata", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: "pg-init", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: pgInitCMName}}}},
					},
				},
			},
		},
	}
	if err := controllerutil.SetControllerReference(cluster, sts, r.Scheme); err != nil {
		return "", false, err
	}
	if err := r.createOrUpdateStatefulSet(ctx, sts); err != nil {
		return "", false, err
	}

	pgSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: pgSvcName, Namespace: cluster.Namespace, Labels: voxClusterLabels(cluster.Name, "postgres")},
		Spec: corev1.ServiceSpec{
			Selector: voxClusterLabels(cluster.Name, "postgres"),
			Ports:    []corev1.ServicePort{{Name: "postgres", Port: pgPort, TargetPort: intstr.FromInt32(pgPort), Protocol: corev1.ProtocolTCP}},
		},
	}
	if err := controllerutil.SetControllerReference(cluster, pgSvc, r.Scheme); err != nil {
		return "", false, err
	}
	if err := r.createOrUpdateService(ctx, pgSvc); err != nil {
		return "", false, err
	}

	ready := false
	current := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: pgSvcName}, current); err == nil {
		ready = current.Status.ReadyReplicas > 0
	}
	return fmt.Sprintf("%s.%s.svc.cluster.local", pgSvcName, cluster.Namespace), ready, nil
}
