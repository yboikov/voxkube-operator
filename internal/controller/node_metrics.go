package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
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

// nodeMetricFixedLabels are on every node metric. The allowlisted VoxNode labels monitoring.voxkube.io/<label>
// (--node-metric-labels) are added after them.
var nodeMetricFixedLabels = []string{"namespace", "voxnode", "certname", "cluster", "role", "state"}

// DefaultNodeMetricLabels is the default --node-metric-labels.
const DefaultNodeMetricLabels = "alerts"

// ParseNodeMetricLabels validates a comma-separated list of monitoring label names for the node metrics.
// Each must be a valid label name that is neither reserved by Prometheus nor one of the fixed labels.
func ParseNodeMetricLabels(csv string) ([]string, error) {
	fixed := map[string]bool{}
	for _, l := range nodeMetricFixedLabels {
		fixed[l] = true
	}
	seen := map[string]bool{}
	var out []string
	for _, name := range strings.Split(csv, ",") {
		name = strings.TrimSpace(name)
		switch {
		case name == "" || seen[name]:
			continue
		case !endpointLabelName.MatchString(name) || len(name) > 63:
			return nil, fmt.Errorf("node metric label %q: use letters, digits and _, starting with a letter and ending with a letter or digit", name)
		case reservedTargetLabels[name] || fixed[name]:
			return nil, fmt.Errorf("node metric label %q is reserved", name)
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

type nodeSample struct {
	metric string
	labels map[string]string
	value  float64
}

// nodeMetricSamples turns VoxNodes into samples carrying the fixed labels plus the allowlisted monitoring labels.
func nodeMetricSamples(nodes []openvoxv1alpha1.VoxNode, extraLabels []string) []nodeSample {
	var out []nodeSample
	for i := range nodes {
		n := &nodes[i]
		state := n.Spec.State
		if state == "" {
			state = openvoxv1alpha1.NodeStateActive // the API default
		}
		labels := map[string]string{
			"namespace": n.Namespace,
			"voxnode":   n.Name,
			"certname":  n.Spec.Certname,
			"cluster":   n.Spec.ClusterRef,
			"role":      n.Status.Role,
			"state":     string(state),
		}
		monitoring := voxNodeMonitoringLabels(n.Labels)
		for _, name := range extraLabels {
			labels[name] = monitoring[name]
		}
		active := state == openvoxv1alpha1.NodeStateActive
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

// NodeMetricsCollector exports nodeMetricSamples on every scrape. The label names are fixed for the life of the
// process: the fixed labels plus Labels (from --node-metric-labels), empty where a node lacks one. Adding a
// monitoring label to one VoxNode therefore never changes the identity of every other series.
// It is an unchecked collector (Describe sends nothing); Collect builds const metrics from the cache.
type NodeMetricsCollector struct {
	Reader client.Reader
	// Labels are the monitoring.voxkube.io/<label> VoxNode labels exported on every series.
	Labels []string
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
	for _, m := range buildNodeMetrics(nodeMetricSamples(list.Items, c.Labels), c.Labels) {
		ch <- m
	}
}

func buildNodeMetrics(samples []nodeSample, extraLabels []string) []prometheus.Metric {
	names := append(append([]string{}, nodeMetricFixedLabels...), extraLabels...)

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
