package controller

import (
	"context"
	"sort"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	openvoxv1alpha1 "github.com/yboikov/voxkube-operator/api/v1alpha1"
)

// Per-VoxNode Puppet run metrics, built from VoxNode status (which the VoxNode controller fills from OpenVox DB).
const (
	metricNodeActive           = "voxkube_node_active"
	metricNodeLastReportFailed = "voxkube_node_last_report_failed"
	metricNodeLastSeen         = "voxkube_node_last_seen_timestamp_seconds"
)

var nodeMetricHelp = map[string]string{
	metricNodeActive:           "1 if the VoxNode's spec.state is Active, 0 otherwise.",
	metricNodeLastReportFailed: "1 if the latest Puppet run of an Active node failed, 0 if it succeeded. Absent until the first report.",
	metricNodeLastSeen:         "Unix time of an Active node's latest report, facts or catalog in OpenVox DB. Absent until the first check-in.",
}

// nodeMetricFixedLabels are on every node metric. VoxNode labels monitoring.voxkube.io/<label> are added after them.
var nodeMetricFixedLabels = []string{"namespace", "voxnode", "certname", "cluster", "role", "state"}

type nodeSample struct {
	metric string
	labels map[string]string
	value  float64
}

// nodeMetricSamples turns VoxNodes into samples. Monitoring labels that are not valid Prometheus label names, are
// reserved, or collide with the fixed labels are left out.
func nodeMetricSamples(nodes []openvoxv1alpha1.VoxNode) []nodeSample {
	fixed := map[string]bool{}
	for _, l := range nodeMetricFixedLabels {
		fixed[l] = true
	}
	var out []nodeSample
	for i := range nodes {
		n := &nodes[i]
		labels := map[string]string{
			"namespace": n.Namespace,
			"voxnode":   n.Name,
			"certname":  n.Spec.Certname,
			"cluster":   n.Spec.ClusterRef,
			"role":      n.Status.Role,
			"state":     string(n.Spec.State),
		}
		for name, v := range voxNodeMonitoringLabels(n.Labels) {
			if endpointLabelName.MatchString(name) && !reservedTargetLabels[name] && !fixed[name] {
				labels[name] = v
			}
		}
		active := n.Spec.State == openvoxv1alpha1.NodeStateActive
		out = append(out, nodeSample{metricNodeActive, labels, boolFloat(active)})
		if !active {
			continue
		}
		if n.Status.LastReportStatus != "" {
			out = append(out, nodeSample{metricNodeLastReportFailed, labels, boolFloat(n.Status.LastReportStatus == "failed")})
		}
		if n.Status.LastSeenTime != nil {
			out = append(out, nodeSample{metricNodeLastSeen, labels, float64(n.Status.LastSeenTime.Unix())})
		}
	}
	return out
}

func boolFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// NodeMetricsCollector exports nodeMetricSamples on every scrape. It is an unchecked collector (Describe sends
// nothing) because the label names depend on the VoxNodes' monitoring labels; within one scrape every metric
// family gets the same label names (the union, empty where a node lacks one), as Prometheus requires.
type NodeMetricsCollector struct {
	Reader client.Reader
}

// Describe implements prometheus.Collector.
func (c *NodeMetricsCollector) Describe(chan<- *prometheus.Desc) {}

// Collect implements prometheus.Collector.
func (c *NodeMetricsCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	list := &openvoxv1alpha1.VoxNodeList{}
	if err := c.Reader.List(ctx, list); err != nil {
		// Typically the cache is still starting; export nothing rather than failing the whole scrape.
		log.Log.V(1).Info("VoxNode metrics skipped", "reason", err.Error())
		return
	}
	for _, m := range buildNodeMetrics(nodeMetricSamples(list.Items)) {
		ch <- m
	}
}

func buildNodeMetrics(samples []nodeSample) []prometheus.Metric {
	extra := map[string]bool{}
	for _, s := range samples {
		for k := range s.labels {
			extra[k] = true
		}
	}
	for _, l := range nodeMetricFixedLabels {
		delete(extra, l)
	}
	names := append([]string{}, nodeMetricFixedLabels...)
	extraNames := make([]string, 0, len(extra))
	for k := range extra {
		extraNames = append(extraNames, k)
	}
	sort.Strings(extraNames)
	names = append(names, extraNames...)

	descs := map[string]*prometheus.Desc{}
	var out []prometheus.Metric
	for _, s := range samples {
		d, ok := descs[s.metric]
		if !ok {
			d = prometheus.NewDesc(s.metric, nodeMetricHelp[s.metric], names, nil)
			descs[s.metric] = d
		}
		values := make([]string, len(names))
		for i, n := range names {
			values[i] = s.labels[n]
		}
		out = append(out, prometheus.MustNewConstMetric(d, prometheus.GaugeValue, s.value, values...))
	}
	return out
}
