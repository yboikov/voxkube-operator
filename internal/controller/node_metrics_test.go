package controller

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	openvoxv1alpha1 "github.com/yboikov/voxkube-operator/api/v1alpha1"
)

func TestNodeMetricsCollector(t *testing.T) {
	seen := metav1.NewTime(time.Unix(1795180000, 0))
	node := func(name string, state openvoxv1alpha1.NodeState, report string, lastSeen *metav1.Time, labels map[string]string) *openvoxv1alpha1.VoxNode {
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
		node("new01", openvoxv1alpha1.NodeStateActive, "", nil, nil),                 // approved, never reported
		node("old01", openvoxv1alpha1.NodeStateDecommissioned, "failed", &seen, nil), // retired: only voxkube_node_active 0
	).Build()

	c := &NodeMetricsCollector{Reader: k8sClient}
	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatal(err)
	}
	want := `
# HELP voxkube_node_active 1 if the VoxNode's spec.state is Active, 0 otherwise.
# TYPE voxkube_node_active gauge
voxkube_node_active{alerts="false",certname="web01.example.com",cluster="openvox",namespace="openvox",role="jumphost",state="Active",voxnode="web01"} 1
voxkube_node_active{alerts="",certname="web02.example.com",cluster="openvox",namespace="openvox",role="jumphost",state="Active",voxnode="web02"} 1
voxkube_node_active{alerts="",certname="new01.example.com",cluster="openvox",namespace="openvox",role="jumphost",state="Active",voxnode="new01"} 1
voxkube_node_active{alerts="",certname="old01.example.com",cluster="openvox",namespace="openvox",role="jumphost",state="Decommissioned",voxnode="old01"} 0
# HELP voxkube_node_last_report_failed 1 if the latest Puppet run of an Active node failed, 0 if it succeeded. Absent until the first report.
# TYPE voxkube_node_last_report_failed gauge
voxkube_node_last_report_failed{alerts="false",certname="web01.example.com",cluster="openvox",namespace="openvox",role="jumphost",state="Active",voxnode="web01"} 1
voxkube_node_last_report_failed{alerts="",certname="web02.example.com",cluster="openvox",namespace="openvox",role="jumphost",state="Active",voxnode="web02"} 0
# HELP voxkube_node_last_seen_timestamp_seconds Unix time of an Active node's latest report, facts or catalog in OpenVox DB. Absent until the first check-in.
# TYPE voxkube_node_last_seen_timestamp_seconds gauge
voxkube_node_last_seen_timestamp_seconds{alerts="false",certname="web01.example.com",cluster="openvox",namespace="openvox",role="jumphost",state="Active",voxnode="web01"} 1.79518e+09
voxkube_node_last_seen_timestamp_seconds{alerts="",certname="web02.example.com",cluster="openvox",namespace="openvox",role="jumphost",state="Active",voxnode="web02"} 1.79518e+09
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want)); err != nil {
		t.Error(err)
	}

	// No VoxNodes: nothing exported, no error.
	empty := &NodeMetricsCollector{Reader: fake.NewClientBuilder().WithScheme(newVoxKubeScheme(t)).Build()}
	if n := testutil.CollectAndCount(empty); n != 0 {
		t.Errorf("expected no metrics without VoxNodes, got %d", n)
	}
}
