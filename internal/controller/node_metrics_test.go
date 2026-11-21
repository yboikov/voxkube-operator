package controller

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	openvoxv1alpha1 "github.com/yboikov/voxkube-operator/api/v1alpha1"
)

func TestNodeMetricsCollector(t *testing.T) {
	seen := metav1.NewTime(time.Unix(1795180000, 0))
	node := func(name string, state openvoxv1alpha1.VoxNodeState, report string, lastSeen *metav1.Time, labels map[string]string) *openvoxv1alpha1.VoxNode {
		return &openvoxv1alpha1.VoxNode{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "openvox", Labels: labels},
			Spec:       openvoxv1alpha1.VoxNodeSpec{ClusterRef: "openvox", Certname: name + ".example.com", State: state},
			Status:     openvoxv1alpha1.VoxNodeStatus{Role: "jumphost", LastReportStatus: report, LastSeenTime: lastSeen},
		}
	}
	k8sClient := fake.NewClientBuilder().WithScheme(newVoxKubeScheme(t)).WithObjects(
		node("web01", openvoxv1alpha1.NodeStateActive, "failed", &seen, map[string]string{
			"monitoring.voxkube.io/alerts": "false",
			"monitoring.voxkube.io/job":    "ignored", // reserved Prometheus label
			"monitoring.voxkube.io/role":   "ignored", // collides with a fixed label
			"app.kubernetes.io/name":       "ignored", // not a monitoring label
		}),
		node("web02", openvoxv1alpha1.NodeStateActive, "changed", &seen, nil),
		node("new01", openvoxv1alpha1.NodeStateActive, "", nil, nil),                                            // approved, never reported
		node("old01", openvoxv1alpha1.NodeStateDecommissioned, "failed", &seen, nil),                            // retired: only voxkube_node_active 0
		node("def01", "", "changed", &seen, map[string]string{"monitoring.voxkube.io/team": "not-allowlisted"}), // empty state = Active
	).Build()

	c := &NodeMetricsCollector{Reader: k8sClient, Labels: []string{"alerts"}}
	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatal(err)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	// got["metric"]["voxnode"] = value; labels["metric"]["voxnode"] = label set
	got := map[string]map[string]float64{}
	labels := map[string]map[string]map[string]string{}
	for _, f := range families {
		got[f.GetName()] = map[string]float64{}
		labels[f.GetName()] = map[string]map[string]string{}
		for _, m := range f.GetMetric() {
			ls := map[string]string{}
			for _, lp := range m.GetLabel() {
				ls[lp.GetName()] = lp.GetValue()
			}
			got[f.GetName()][ls["voxnode"]] = m.GetGauge().GetValue()
			labels[f.GetName()][ls["voxnode"]] = ls
		}
	}

	want := map[string]map[string]float64{
		metricNodeActive:           {"web01": 1, "web02": 1, "new01": 1, "old01": 0, "def01": 1},
		metricNodeLastReportFailed: {"web01": 1, "web02": 0, "def01": 0},
		metricNodeLastSeen:         {"web01": 1795180000, "web02": 1795180000, "def01": 1795180000},
	}
	for metric, nodes := range want {
		if len(got[metric]) != len(nodes) {
			t.Errorf("%s: got %v, want %v", metric, got[metric], nodes)
		}
		for n, v := range nodes {
			if g, ok := got[metric][n]; !ok || g != v {
				t.Errorf("%s{voxnode=%q} = %v (present %v), want %v", metric, n, g, ok, v)
			}
		}
	}

	l := labels[metricNodeLastReportFailed]["web01"]
	if l["certname"] != "web01.example.com" || l["role"] != "jumphost" || l["state"] != "Active" || l["cluster"] != "openvox" || l["alerts"] != "false" {
		t.Errorf("unexpected labels on web01: %v", l)
	}
	if l := labels[metricNodeActive]["def01"]; l["state"] != "Active" {
		t.Errorf("an empty spec.state is Active, got state=%q", l["state"])
	}
	if _, ok := labels[metricNodeActive]["def01"]["team"]; ok {
		t.Error("labels outside --node-metric-labels must not be exported")
	}
	if _, ok := l["job"]; ok {
		t.Error("reserved label job must not be copied from monitoring labels")
	}
	if _, ok := l["name"]; ok {
		t.Error("labels outside monitoring.voxkube.io/ must not be copied")
	}
	if l2 := labels[metricNodeLastReportFailed]["web02"]; l2["alerts"] != "" {
		t.Errorf("every series in a family needs the same label names; web02 should have alerts=\"\", got %v", l2)
	}

	// No VoxNodes: nothing exported, no error.
	empty := &NodeMetricsCollector{Reader: fake.NewClientBuilder().WithScheme(newVoxKubeScheme(t)).Build()}
	reg2 := prometheus.NewRegistry()
	if err := reg2.Register(empty); err != nil {
		t.Fatal(err)
	}
	if fams, err := reg2.Gather(); err != nil || len(fams) != 0 {
		t.Errorf("expected no metrics without VoxNodes, got %d families (err %v)", len(fams), err)
	}
}

func TestParseNodeMetricLabels(t *testing.T) {
	got, err := ParseNodeMetricLabels(" team, alerts,,team ")
	if err != nil || len(got) != 2 || got[0] != "alerts" || got[1] != "team" {
		t.Errorf("expected [alerts team], got %v (err %v)", got, err)
	}
	for _, bad := range []string{"job", "role", "_x", "x_", "a-b"} {
		if _, err := ParseNodeMetricLabels(bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
	if got, err := ParseNodeMetricLabels(""); err != nil || len(got) != 0 {
		t.Errorf("an empty list is allowed, got %v (err %v)", got, err)
	}
}
