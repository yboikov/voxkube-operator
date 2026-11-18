package controller

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	openvoxv1alpha1 "github.com/yboikov/voxkube-operator/api/v1alpha1"
)

const (
	// MaxEndpointsPerSlice is the maximum number of endpoints placed in a single Kubernetes EndpointSlice.
	MaxEndpointsPerSlice = 100
)

var nonDNSChars = regexp.MustCompile(`[^a-z0-9-]`)

// VoxDiscoveryReconciler reconciles a VoxDiscovery object.
type VoxDiscoveryReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
	DBClient OpenVoxDBClient
}

// +kubebuilder:rbac:groups=voxkube.io,resources=voxdiscoveries,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=voxkube.io,resources=voxdiscoveries/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=voxkube.io,resources=voxdiscoveries/finalizers,verbs=update
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors,verbs=get;list;watch;create;update;patch;delete

// Reconcile queries OpenVox DB (PuppetDB) for active VMs matching the selector and exports them
// into a headless Service, chunked EndpointSlices (<= 100 endpoints per slice), and an optional
// mTLS-configured Prometheus ServiceMonitor.
func (r *VoxDiscoveryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	disc := &openvoxv1alpha1.VoxDiscovery{}
	if err := r.Get(ctx, req.NamespacedName, disc); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if err := validateTargetLabels(disc.Spec.ServiceMonitor.TargetLabels); err != nil {
		meta.SetStatusCondition(&disc.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: disc.Generation,
			Reason:             "InvalidTargetLabels",
			Message:            err.Error(),
		})
		_ = r.Status().Update(ctx, disc)
		return ctrl.Result{}, nil
	}

	cluster := &openvoxv1alpha1.VoxCluster{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: disc.Namespace, Name: disc.Spec.ClusterRef}, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			meta.SetStatusCondition(&disc.Status.Conditions, metav1.Condition{
				Type:               "Ready",
				Status:             metav1.ConditionFalse,
				ObservedGeneration: disc.Generation,
				Reason:             "ClusterNotFound",
				Message:            fmt.Sprintf("VoxCluster %q not found", disc.Spec.ClusterRef),
			})
			_ = r.Status().Update(ctx, disc)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Without OpenVox DB there is nothing to discover: remove what this VoxDiscovery created and wait until the
	// VoxCluster enables its database (the VoxCluster watch re-triggers this reconcile).
	if !databaseEnabled(cluster) {
		if err := r.removeDiscoveryResources(ctx, disc); err != nil {
			return ctrl.Result{}, err
		}
		disc.Status.DiscoveredNodes = 0
		disc.Status.EndpointSliceCount = 0
		disc.Status.ServiceName = ""
		disc.Status.ScraperSecretName = ""
		meta.SetStatusCondition(&disc.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: disc.Generation,
			Reason:             "DatabaseDisabled",
			Message:            fmt.Sprintf("VoxCluster %q has spec.database.enabled=false; nothing to discover", cluster.Name),
		})
		return ctrl.Result{}, r.Status().Update(ctx, disc)
	}

	// 1. Reconcile headless Service (ClusterIP: None)
	svcName, err := r.reconcileHeadlessService(ctx, disc)
	if err != nil {
		logger.Error(err, "failed to reconcile headless Service for VoxDiscovery")
		return ctrl.Result{}, err
	}

	// 2. Query OpenVox DB (PuppetDB) for active nodes matching selector
	var activeNodes []DiscoveredPuppetNode
	queryErr := ""
	if dbClient := resolveDBClient(ctx, r.Client, r.DBClient, cluster); dbClient != nil {
		dbURL := cluster.Status.DatabaseURL
		if dbURL == "" {
			dbURL = dbServiceURL(cluster)
		}
		nodes, err := dbClient.QueryActiveNodes(ctx, dbURL, disc.Spec.Selector)
		if err != nil {
			logger.Info("OpenVox DB query failed; keeping EndpointSlices as they are and requeuing", "dbURL", dbURL, "reason", err.Error())
			queryErr = err.Error()
		} else {
			activeNodes = nodes
		}
	}

	// Also filter out any VoxNode in the namespace that is explicitly Decommissioned or Revoked
	decommissionedSet := make(map[string]bool)
	voxNodeList := &openvoxv1alpha1.VoxNodeList{}
	if err := r.List(ctx, voxNodeList, client.InNamespace(disc.Namespace)); err == nil {
		for _, vn := range voxNodeList.Items {
			if vn.Spec.ClusterRef == cluster.Name &&
				(vn.Spec.State == openvoxv1alpha1.NodeStateDecommissioned || vn.Spec.State == openvoxv1alpha1.NodeStateRevoked) {
				decommissionedSet[vn.Spec.Certname] = true
			}
		}
	}

	syncInterval := 60 * time.Second
	if disc.Spec.SyncInterval != "" {
		if d, err := time.ParseDuration(disc.Spec.SyncInterval); err == nil && d > 0 {
			syncInterval = d
		}
	}

	// A failed query must not wipe the targets Prometheus is scraping: keep the last good slices.
	if queryErr != "" {
		meta.SetStatusCondition(&disc.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: disc.Generation,
			Reason:             "QueryFailed",
			Message:            queryErr,
		})
		_ = r.Status().Update(ctx, disc)
		return ctrl.Result{RequeueAfter: syncInterval}, nil
	}

	// 3. Reconcile chunked EndpointSlices (<= 100 endpoints per slice, one set per IP family)
	discoveredCount, sliceCount, err := r.reconcileEndpointSlices(ctx, disc, svcName, activeNodes, decommissionedSet)
	if err != nil {
		logger.Error(err, "failed to reconcile EndpointSlices")
		return ctrl.Result{}, err
	}

	// 4. Reconcile optional Prometheus ServiceMonitor + zero-touch OpenVox CA mTLS scraper cert
	var scraperSecName string
	if disc.Spec.ServiceMonitor.Enabled {
		scraperSecName, err = r.reconcileServiceMonitor(ctx, disc, cluster, svcName)
		if err != nil {
			logger.Error(err, "failed to reconcile Prometheus ServiceMonitor")
			return ctrl.Result{}, err
		}
	}

	now := metav1.Now()
	disc.Status.DiscoveredNodes = discoveredCount
	disc.Status.EndpointSliceCount = sliceCount
	disc.Status.ServiceName = svcName
	disc.Status.ScraperSecretName = scraperSecName
	disc.Status.LastSyncTime = &now

	meta.SetStatusCondition(&disc.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: disc.Generation,
		Reason:             "Synced",
		Message:            fmt.Sprintf("Exported %d active nodes across %d EndpointSlice(s)", discoveredCount, sliceCount),
	})

	desiredStatus := disc.Status
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &openvoxv1alpha1.VoxDiscovery{}
		if err := r.Get(ctx, req.NamespacedName, latest); err != nil {
			return err
		}
		latest.Status = desiredStatus
		return r.Status().Update(ctx, latest)
	}); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: syncInterval}, nil
}

func (r *VoxDiscoveryReconciler) reconcileHeadlessService(ctx context.Context, disc *openvoxv1alpha1.VoxDiscovery) (string, error) {
	svcName := disc.Name
	var svcPorts []corev1.ServicePort
	for _, p := range disc.Spec.Ports {
		proto := p.Protocol
		if proto == "" {
			proto = corev1.ProtocolTCP
		}
		svcPorts = append(svcPorts, corev1.ServicePort{
			Name:     p.Name,
			Port:     p.Port,
			Protocol: proto,
		})
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svcName,
			Namespace: disc.Namespace,
			Labels:    discoveryServiceLabels(disc),
		},
		Spec: corev1.ServiceSpec{
			ClusterIP: corev1.ClusterIPNone,
			Ports:     svcPorts,
		},
	}
	if err := controllerutil.SetControllerReference(disc, svc, r.Scheme); err != nil {
		return "", err
	}

	existing := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Namespace: disc.Namespace, Name: svcName}, existing)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, svc); err != nil {
			return "", err
		}
		return svcName, nil
	}
	if err != nil {
		return "", err
	}
	existing.Spec.Ports = svc.Spec.Ports
	existing.Labels = svc.Labels
	if err := r.Update(ctx, existing); err != nil {
		return "", err
	}
	return svcName, nil
}

func (r *VoxDiscoveryReconciler) reconcileEndpointSlices(
	ctx context.Context,
	disc *openvoxv1alpha1.VoxDiscovery,
	svcName string,
	nodes []DiscoveredPuppetNode,
	decommissionedSet map[string]bool,
) (discoveredCount int32, sliceCount int32, err error) {
	addrFact := disc.Spec.AddressFact
	if addrFact == "" {
		addrFact = "networking.ip"
	}

	// Sort nodes deterministically by certname
	sort.Slice(nodes, func(i, j int) bool {
		return nodes[i].Certname < nodes[j].Certname
	})

	var endpointsV4, endpointsV6 []discoveryv1.Endpoint
	for _, n := range nodes {
		if decommissionedSet[n.Certname] {
			continue
		}
		ip := net.ParseIP(LookupFactString(n.Facts, addrFact))
		if ip == nil {
			continue
		}

		hostname := sanitizeDNSLabel(n.Certname)
		nodeName := n.Certname
		ep := discoveryv1.Endpoint{
			Addresses:  []string{ip.String()},
			Hostname:   &hostname,
			NodeName:   &nodeName,
			Conditions: discoveryv1.EndpointConditions{Ready: new(true)},
		}
		if ip.To4() != nil {
			endpointsV4 = append(endpointsV4, ep)
		} else {
			endpointsV6 = append(endpointsV6, ep)
		}
	}

	var slicePorts []discoveryv1.EndpointPort
	for _, p := range disc.Spec.Ports {
		proto := p.Protocol
		if proto == "" {
			proto = corev1.ProtocolTCP
		}
		name := p.Name
		port := p.Port
		slicePorts = append(slicePorts, discoveryv1.EndpointPort{
			Name:     &name,
			Port:     &port,
			Protocol: &proto,
		})
	}

	type sliceSpec struct {
		name      string
		family    discoveryv1.AddressType
		endpoints []discoveryv1.Endpoint
	}
	chunk := func(eps []discoveryv1.Endpoint, prefix string, family discoveryv1.AddressType) []sliceSpec {
		var out []sliceSpec
		for i, idx := 0, 0; i < len(eps); i, idx = i+MaxEndpointsPerSlice, idx+1 {
			end := i + MaxEndpointsPerSlice
			if end > len(eps) {
				end = len(eps)
			}
			out = append(out, sliceSpec{name: fmt.Sprintf("%s%d", prefix, idx), family: family, endpoints: eps[i:end]})
		}
		return out
	}
	// IPv4 slices keep the historical "<svc>-<n>" names; IPv6 slices are "<svc>-v6-<n>".
	slices := chunk(endpointsV4, svcName+"-", discoveryv1.AddressTypeIPv4)
	slices = append(slices, chunk(endpointsV6, svcName+"-v6-", discoveryv1.AddressTypeIPv6)...)
	if len(slices) == 0 {
		// Keep one empty EndpointSlice so the headless Service has a valid slice.
		slices = []sliceSpec{{name: svcName + "-0", family: discoveryv1.AddressTypeIPv4}}
	}

	desiredSliceNames := make(map[string]bool)
	for _, sl := range slices {
		sliceName := sl.name
		desiredSliceNames[sliceName] = true

		labels := voxDiscoveryLabels(disc.Name)
		labels[discoveryv1.LabelServiceName] = svcName
		labels[discoveryv1.LabelManagedBy] = "voxkube-operator"

		slice := &discoveryv1.EndpointSlice{
			ObjectMeta: metav1.ObjectMeta{
				Name:      sliceName,
				Namespace: disc.Namespace,
				Labels:    labels,
			},
			AddressType: sl.family,
			Endpoints:   sl.endpoints,
			Ports:       slicePorts,
		}
		if err := controllerutil.SetControllerReference(disc, slice, r.Scheme); err != nil {
			return 0, 0, err
		}

		existing := &discoveryv1.EndpointSlice{}
		getErr := r.Get(ctx, types.NamespacedName{Namespace: disc.Namespace, Name: sliceName}, existing)
		if apierrors.IsNotFound(getErr) {
			if err := r.Create(ctx, slice); err != nil {
				return 0, 0, err
			}
		} else if getErr == nil {
			if existing.AddressType != sl.family {
				// addressType is immutable; recreate the slice.
				if err := r.Delete(ctx, existing); err != nil && !apierrors.IsNotFound(err) {
					return 0, 0, err
				}
				if err := r.Create(ctx, slice); err != nil {
					return 0, 0, err
				}
				continue
			}
			existing.Endpoints = slice.Endpoints
			existing.Ports = slice.Ports
			existing.Labels = slice.Labels
			if err := r.Update(ctx, existing); err != nil {
				return 0, 0, err
			}
		} else {
			return 0, 0, getErr
		}
	}

	// Prune stale EndpointSlices if the node count shrank
	existingSlices := &discoveryv1.EndpointSliceList{}
	if err := r.List(ctx, existingSlices, client.InNamespace(disc.Namespace), client.MatchingLabels{
		discoveryv1.LabelServiceName: svcName,
	}); err == nil {
		for i := range existingSlices.Items {
			s := &existingSlices.Items[i]
			if !desiredSliceNames[s.Name] {
				_ = r.Delete(ctx, s)
			}
		}
	}

	return int32(len(endpointsV4) + len(endpointsV6)), int32(len(slices)), nil
}

func (r *VoxDiscoveryReconciler) reconcileServiceMonitor(
	ctx context.Context,
	disc *openvoxv1alpha1.VoxDiscovery,
	cluster *openvoxv1alpha1.VoxCluster,
	svcName string,
) (string, error) {
	var scraperSecName string

	// If TLS mode is openvoxCA, mint a prometheus-scraper client certificate signed by the OpenVox CA
	if disc.Spec.ServiceMonitor.TLS.Enabled {
		if disc.Spec.ServiceMonitor.TLS.Mode == "" || disc.Spec.ServiceMonitor.TLS.Mode == "openvoxCA" {
			var err error
			scraperSecName, err = r.reconcilePrometheusScraperCert(ctx, disc, cluster)
			if err != nil {
				return "", err
			}
		} else {
			scraperSecName = disc.Spec.ServiceMonitor.TLS.SecretName
		}
	}

	smObj := &unstructured.Unstructured{}
	smObj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "monitoring.coreos.com",
		Version: "v1",
		Kind:    "ServiceMonitor",
	})
	smObj.SetName(disc.Name)
	smObj.SetNamespace(disc.Namespace)

	labels := voxDiscoveryLabels(disc.Name)
	for k, v := range disc.Spec.ServiceMonitor.AdditionalLabels {
		labels[k] = v
	}
	smObj.SetLabels(labels)

	smObj.Object["spec"] = buildServiceMonitorSpec(disc, scraperSecName)

	if err := controllerutil.SetControllerReference(disc, smObj, r.Scheme); err != nil {
		return scraperSecName, err
	}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(smObj.GroupVersionKind())
	err := r.Get(ctx, types.NamespacedName{Namespace: disc.Namespace, Name: disc.Name}, existing)
	if apierrors.IsNotFound(err) {
		if createErr := r.Create(ctx, smObj); createErr != nil {
			if !meta.IsNoMatchError(createErr) && !strings.Contains(createErr.Error(), "no matches for kind \"ServiceMonitor\"") {
				return scraperSecName, createErr
			}
		}
	} else if err == nil {
		smObj.SetResourceVersion(existing.GetResourceVersion())
		if updErr := r.Update(ctx, smObj); updErr != nil {
			return scraperSecName, updErr
		}
	}
	// Any other Get error (typically: ServiceMonitor CRD not installed) leaves monitoring disabled.

	return scraperSecName, nil
}

// buildServiceMonitorSpec renders the ServiceMonitor spec for a VoxDiscovery.
//
// The operator publishes targets only as EndpointSlices (the headless Service has no selector, and Kubernetes
// never mirrors EndpointSlices back into Endpoints), so the ServiceMonitor must use the EndpointSlice discovery
// role. Prometheus Operator defaults to the Endpoints role, which would find zero targets.
func buildServiceMonitorSpec(disc *openvoxv1alpha1.VoxDiscovery, scraperSecName string) map[string]interface{} {
	interval := disc.Spec.ServiceMonitor.Interval
	if interval == "" {
		interval = "30s"
	}
	scrapeTimeout := disc.Spec.ServiceMonitor.ScrapeTimeout
	if scrapeTimeout == "" {
		scrapeTimeout = "10s"
	}

	var smEndpoints []interface{}
	for _, p := range disc.Spec.Ports {
		ep := map[string]interface{}{
			"port":          p.Name,
			"interval":      interval,
			"scrapeTimeout": scrapeTimeout,
		}
		if disc.Spec.ServiceMonitor.PreserveNodeHostnameAsInstance == nil || *disc.Spec.ServiceMonitor.PreserveNodeHostnameAsInstance {
			// nodeName on each endpoint is the Puppet certname; only a non-empty value replaces the default
			// instance (<ip>:<port>).
			ep["relabelings"] = []interface{}{
				map[string]interface{}{
					"sourceLabels": []interface{}{"__meta_kubernetes_endpointslice_endpoint_node_name"},
					"regex":        "(.+)",
					"targetLabel":  "instance",
					"replacement":  "$1",
					"action":       "replace",
				},
			}
		}
		if disc.Spec.ServiceMonitor.TLS.Enabled && scraperSecName != "" {
			ep["scheme"] = "https"
			ep["tlsConfig"] = map[string]interface{}{
				"ca":        map[string]interface{}{"secret": map[string]interface{}{"name": scraperSecName, "key": "ca.crt"}},
				"cert":      map[string]interface{}{"secret": map[string]interface{}{"name": scraperSecName, "key": "tls.crt"}},
				"keySecret": map[string]interface{}{"name": scraperSecName, "key": "tls.key"},
			}
		}
		smEndpoints = append(smEndpoints, ep)
	}

	matchLabels := make(map[string]interface{})
	for k, v := range voxDiscoveryLabels(disc.Name) {
		matchLabels[k] = v
	}

	spec := map[string]interface{}{
		"serviceDiscoveryRole": "EndpointSlice",
		"selector":             map[string]interface{}{"matchLabels": matchLabels},
		"endpoints":            smEndpoints,
	}
	if keys := sortedKeys(disc.Spec.ServiceMonitor.TargetLabels); len(keys) > 0 {
		spec["targetLabels"] = toInterfaceSlice(keys)
	}
	return spec
}

// reservedTargetLabels are set by Prometheus or Prometheus Operator on every ServiceMonitor target.
var reservedTargetLabels = map[string]bool{
	"job": true, "instance": true, "namespace": true, "service": true, "endpoint": true, "pod": true, "container": true,
}

var prometheusLabelName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// validateTargetLabels checks that serviceMonitor.targetLabels can be both Service labels and Prometheus labels.
func validateTargetLabels(labels map[string]string) error {
	for _, k := range sortedKeys(labels) {
		switch {
		case !prometheusLabelName.MatchString(k) || strings.HasPrefix(k, "__"):
			return fmt.Errorf("serviceMonitor.targetLabels: %q is not a valid Prometheus label name", k)
		case reservedTargetLabels[k]:
			return fmt.Errorf("serviceMonitor.targetLabels: %q is reserved by Prometheus", k)
		}
		if errs := validation.IsValidLabelValue(labels[k]); len(errs) > 0 {
			return fmt.Errorf("serviceMonitor.targetLabels[%s]: %s", k, strings.Join(errs, "; "))
		}
	}
	return nil
}

// discoveryServiceLabels are the headless Service labels: serviceMonitor.targetLabels plus the selector labels
// (which cannot collide: Prometheus label names contain no "." or "/").
func discoveryServiceLabels(disc *openvoxv1alpha1.VoxDiscovery) map[string]string {
	labels := make(map[string]string, len(disc.Spec.ServiceMonitor.TargetLabels)+3)
	for k, v := range disc.Spec.ServiceMonitor.TargetLabels {
		labels[k] = v
	}
	for k, v := range voxDiscoveryLabels(disc.Name) {
		labels[k] = v
	}
	return labels
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (r *VoxDiscoveryReconciler) reconcilePrometheusScraperCert(
	ctx context.Context,
	disc *openvoxv1alpha1.VoxDiscovery,
	cluster *openvoxv1alpha1.VoxCluster,
) (string, error) {
	secName := fmt.Sprintf("%s-prometheus-scraper-tls", disc.Name)

	caSecName := caSecretName(cluster)
	caSec := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: disc.Namespace, Name: caSecName}, caSec); err != nil {
		return "", err
	}
	caCert := caSec.Data["ca.crt"]
	caKey := caSec.Data["ca.key"]
	if len(caCert) == 0 || len(caKey) == 0 {
		return "", fmt.Errorf("CA Secret %s is missing ca.crt or ca.key", caSecName)
	}

	existing := &corev1.Secret{}
	existingErr := r.Get(ctx, types.NamespacedName{Namespace: disc.Namespace, Name: secName}, existing)
	if existingErr == nil {
		if renew, _ := LeafNeedsRenewal(existing.Data["tls.crt"], caCert, nil, leafRenewBefore); !renew {
			return secName, nil
		}
	} else if !apierrors.IsNotFound(existingErr) {
		return "", existingErr
	}

	// The scraper certificate deliberately has no pp_cli_auth extension: it must not be able to use the CA admin API.
	clientCert, clientKey, err := IssueLeafCertificate(caCert, caKey, LeafCertOptions{
		CommonName: "prometheus-scraper",
		DNSNames:   []string{"prometheus-scraper"},
		Duration:   365 * 24 * time.Hour,
	})
	if err != nil {
		return "", err
	}

	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secName,
			Namespace: disc.Namespace,
			Labels:    voxDiscoveryLabels(disc.Name),
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			"ca.crt":  caCert,
			"tls.crt": clientCert,
			"tls.key": clientKey,
		},
	}
	if err := controllerutil.SetControllerReference(disc, sec, r.Scheme); err != nil {
		return "", err
	}
	if existingErr == nil {
		existing.Data = sec.Data
		if err := r.Update(ctx, existing); err != nil {
			return "", err
		}
		return secName, nil
	}
	if err := r.Create(ctx, sec); err != nil {
		return "", err
	}
	return secName, nil
}

func sanitizeDNSLabel(certname string) string {
	lower := strings.ToLower(certname)
	parts := strings.Split(lower, ".")
	first := nonDNSChars.ReplaceAllString(parts[0], "-")
	first = strings.Trim(first, "-")
	if len(first) > 63 {
		first = first[:63]
	}
	if first == "" {
		return "puppet-node"
	}
	return first
}

func voxDiscoveryLabels(name string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "voxkube-discovery",
		"app.kubernetes.io/instance":   name,
		"app.kubernetes.io/managed-by": "voxkube-operator",
	}
}

// removeDiscoveryResources deletes the headless Service, EndpointSlices and ServiceMonitor owned by disc.
// The scraper TLS Secret is kept so that re-enabling discovery reuses the certificate.
func (r *VoxDiscoveryReconciler) removeDiscoveryResources(ctx context.Context, disc *openvoxv1alpha1.VoxDiscovery) error {
	slices := &discoveryv1.EndpointSliceList{}
	if err := r.List(ctx, slices, client.InNamespace(disc.Namespace), client.MatchingLabels{
		discoveryv1.LabelServiceName: disc.Name,
	}); err != nil {
		return err
	}
	for i := range slices.Items {
		if metav1.IsControlledBy(&slices.Items[i], disc) {
			if err := r.Delete(ctx, &slices.Items[i]); client.IgnoreNotFound(err) != nil {
				return err
			}
		}
	}

	svc := &corev1.Service{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: disc.Namespace, Name: disc.Name}, svc); err == nil {
		if metav1.IsControlledBy(svc, disc) {
			if err := r.Delete(ctx, svc); client.IgnoreNotFound(err) != nil {
				return err
			}
		}
	} else if !apierrors.IsNotFound(err) {
		return err
	}

	sm := &unstructured.Unstructured{}
	sm.SetGroupVersionKind(schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1", Kind: "ServiceMonitor"})
	if err := r.Get(ctx, types.NamespacedName{Namespace: disc.Namespace, Name: disc.Name}, sm); err == nil {
		if metav1.IsControlledBy(sm, disc) {
			if err := r.Delete(ctx, sm); client.IgnoreNotFound(err) != nil {
				return err
			}
		}
	} else if !apierrors.IsNotFound(err) && !isNoMatch(err, "ServiceMonitor") {
		return err
	}
	return nil
}

// SetupWithManager registers VoxDiscoveryReconciler with the controller manager.
func (r *VoxDiscoveryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&openvoxv1alpha1.VoxDiscovery{}).
		Owns(&corev1.Service{}).
		Owns(&discoveryv1.EndpointSlice{}).
		Owns(&corev1.Secret{}).
		Watches(&openvoxv1alpha1.VoxCluster{}, handler.EnqueueRequestsFromMapFunc(r.discoveriesForCluster)).
		Complete(r)
}

// discoveriesForCluster enqueues every VoxDiscovery that references the changed VoxCluster
// (e.g. when its database is enabled or disabled).
func (r *VoxDiscoveryReconciler) discoveriesForCluster(ctx context.Context, obj client.Object) []reconcile.Request {
	list := &openvoxv1alpha1.VoxDiscoveryList{}
	if err := r.List(ctx, list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, d := range list.Items {
		if d.Spec.ClusterRef == obj.GetName() {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: d.Namespace, Name: d.Name}})
		}
	}
	return reqs
}
