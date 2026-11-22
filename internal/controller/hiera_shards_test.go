package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	openvoxv1alpha1 "github.com/yboikov/voxkube-operator/api/v1alpha1"
)

func TestShardHieraBundleIsAPartition(t *testing.T) {
	bundle := map[string][]byte{"common.yaml": []byte("c")}
	for i := 0; i < 300; i++ {
		bundle[fmt.Sprintf("nodes_web%03d.example.com.yaml", i)] = []byte("n")
	}
	for i := 0; i < 20; i++ {
		bundle[fmt.Sprintf("roles_role%d.yaml", i)] = []byte("r")
	}
	for _, n := range []int{1, 2, 4, 8, 32} {
		shards := shardHieraBundle(bundle, n)
		if len(shards) != n {
			t.Fatalf("n=%d: got %d shards", n, len(shards))
		}
		if _, ok := shards[0]["common.yaml"]; !ok {
			t.Errorf("n=%d: common.yaml must be in shard 0", n)
		}
		seen := map[string]int{}
		for i, sh := range shards {
			for f := range sh {
				if prev, dup := seen[f]; dup {
					t.Errorf("n=%d: %s in shards %d and %d", n, f, prev, i)
				}
				seen[f] = i
			}
			// Sequential host names still spread: no shard is empty with 300 nodes.
			if len(sh) == 0 {
				t.Errorf("n=%d: shard %d is empty", n, i)
			}
		}
		if len(seen) != len(bundle) {
			t.Errorf("n=%d: %d of %d files placed", n, len(seen), len(bundle))
		}
	}
	// Placement is stable for a given shard count.
	if hieraShardIndex("nodes_a.example.com.yaml", 8) != hieraShardIndex("nodes_a.example.com.yaml", 8) {
		t.Error("shard index must be deterministic")
	}
}

func TestHieraShardNames(t *testing.T) {
	c := pkiCluster()
	if got := hieraShardNames(c); len(got) != 1 || got[0] != "prod-hiera-secrets" {
		t.Errorf("one shard keeps the pre-0.3.0 name, got %v", got)
	}
	c.Spec.HieraSecrets.Shards = 4
	want := []string{"prod-hiera-secrets-s4-0", "prod-hiera-secrets-s4-1", "prod-hiera-secrets-s4-2", "prod-hiera-secrets-s4-3"}
	if got := hieraShardNames(c); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, want %v", got, want)
	}
}

func hieraSecretNamesIn(t *testing.T, c client.Client) []string {
	t.Helper()
	list := &corev1.SecretList{}
	if err := c.List(context.Background(), list, client.InNamespace("ovx"),
		client.MatchingLabels(voxClusterLabels("prod", "hiera-secrets"))); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, s := range list.Items {
		out = append(out, s.Name)
	}
	sort.Strings(out)
	return out
}

// setServerRollout marks the compiler Deployment as rolled out or as still rolling.
func setServerRollout(t *testing.T, c client.Client, done bool) {
	t.Helper()
	d := &appsv1.Deployment{}
	getObj(t, c, "prod-server", d)
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	d.Status.ObservedGeneration = d.Generation
	d.Status.Replicas = want
	d.Status.UpdatedReplicas = want
	if !done {
		d.Status.UpdatedReplicas = 0
	}
	// Deployments have a status subresource in the fake client too: Update would drop the status.
	if err := c.Status().Update(context.Background(), d); err != nil {
		t.Fatal(err)
	}
}

func mountedHieraShards(t *testing.T, c client.Client) []string {
	t.Helper()
	d := &appsv1.Deployment{}
	getObj(t, c, "prod-server", d)
	for _, v := range d.Spec.Template.Spec.Volumes {
		if v.Name != "hiera-secrets" {
			continue
		}
		if v.Secret != nil {
			return []string{v.Secret.SecretName}
		}
		var out []string
		for _, p := range v.Projected.Sources {
			out = append(out, p.Secret.Name)
		}
		return out
	}
	return nil
}

// TestHieraShardResize walks 1→4→8→4→1: each new set is written next to the old one, mounted by the compilers,
// and the old set is deleted only once the compilers have rolled out.
func TestHieraShardResize(t *testing.T) {
	s := newVoxKubeScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(pkiCluster()).WithStatusSubresource(&openvoxv1alpha1.VoxCluster{}).Build()
	r := &VoxClusterReconciler{Client: c, Scheme: s, GitPoller: &mockGitPoller{sha: "x"}}
	reconcilePKICluster(t, c, r)
	setServerRollout(t, c, true)
	reconcilePKICluster(t, c, r)

	prev := []string{"prod-hiera-secrets"}
	if got := hieraSecretNamesIn(t, c); strings.Join(got, ",") != strings.Join(prev, ",") {
		t.Fatalf("initial set: %v", got)
	}
	for _, n := range []int32{4, 8, 4, 1} {
		cl := &openvoxv1alpha1.VoxCluster{}
		getObj(t, c, "prod", cl)
		cl.Spec.HieraSecrets.Shards = n
		if err := c.Update(context.Background(), cl); err != nil {
			t.Fatal(err)
		}
		next := hieraShardNames(cl)

		setServerRollout(t, c, false) // the template change is rolling out
		reconcilePKICluster(t, c, r)
		both := append(append([]string{}, prev...), next...)
		sort.Strings(both)
		if got := hieraSecretNamesIn(t, c); strings.Join(got, ",") != strings.Join(both, ",") {
			t.Errorf("shards=%d mid-rollout: want old and new sets %v, got %v", n, both, got)
		}
		if got := mountedHieraShards(t, c); strings.Join(got, ",") != strings.Join(next, ",") {
			t.Errorf("shards=%d: compilers mount %v, want %v", n, got, next)
		}

		setServerRollout(t, c, true)
		reconcilePKICluster(t, c, r)
		want := append([]string{}, next...)
		sort.Strings(want)
		if got := hieraSecretNamesIn(t, c); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("shards=%d after rollout: want %v, got %v", n, want, got)
		}

		getObj(t, c, "prod", cl)
		hs := cl.Status.HieraSecrets
		if hs == nil || hs.Shards != n || strings.Join(hs.SecretNames, ",") != strings.Join(next, ",") {
			t.Errorf("shards=%d: status.hieraSecrets = %+v", n, hs)
		}
		if wantName := map[bool]string{true: "prod-hiera-secrets", false: ""}[n == 1]; cl.Status.HieraSecretName != wantName {
			t.Errorf("shards=%d: status.hieraSecretName = %q, want %q", n, cl.Status.HieraSecretName, wantName)
		}
		if cond := meta.FindStatusCondition(cl.Status.Conditions, ConditionHieraSecretsCapacity); cond == nil || cond.Status != "True" {
			t.Errorf("shards=%d: HieraSecretsCapacity should be True, got %+v", n, cond)
		}
		prev = next
	}
}

func TestHieraCapacityWarning(t *testing.T) {
	rec := events.NewFakeRecorder(10)
	r := &VoxClusterReconciler{Recorder: rec}
	cluster := pkiCluster()
	status := cluster.Status.DeepCopy()
	setCond := func(typ string, ok bool, reason, msg string) {
		st := "False"
		if ok {
			st = "True"
		}
		meta.SetStatusCondition(&status.Conditions, metaCondition(typ, st, reason, msg))
	}

	r.setHieraCapacity(cluster, status, &hieraLayout{names: []string{"prod-hiera-secrets"}, largestBytes: 900 << 10}, setCond)
	cond := meta.FindStatusCondition(status.Conditions, ConditionHieraSecretsCapacity)
	if cond == nil || cond.Status != "False" || cond.Reason != "NearLimit" {
		t.Fatalf("87%% full must be NearLimit, got %+v", cond)
	}
	select {
	case e := <-rec.Events:
		if !strings.Contains(e, "Warning") || !strings.Contains(e, "HieraSecretsNearLimit") {
			t.Errorf("unexpected event %q", e)
		}
	default:
		t.Error("expected a Warning event")
	}

	// Already False: no repeated event on every reconcile.
	cluster.Status = *status
	r.setHieraCapacity(cluster, status, &hieraLayout{names: []string{"prod-hiera-secrets"}, largestBytes: 900 << 10}, setCond)
	select {
	case e := <-rec.Events:
		t.Errorf("event repeated: %q", e)
	default:
	}

	r.setHieraCapacity(cluster, status, &hieraLayout{names: []string{"a", "b"}, largestBytes: 100 << 10}, setCond)
	if cond := meta.FindStatusCondition(status.Conditions, ConditionHieraSecretsCapacity); cond.Status != "True" {
		t.Errorf("9%% full must be True, got %+v", cond)
	}
}

func TestHieraShardOverLimitWritesNothing(t *testing.T) {
	s := newVoxKubeScheme(t)
	cluster := pkiCluster()
	cluster.Spec.HieraSecrets.Shards = 2
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cluster).Build()
	r := &VoxClusterReconciler{Client: c, Scheme: s}
	bundle := map[string][]byte{"common.yaml": make([]byte, maxSecretDataBytes+1)}
	layout, err := r.writeHieraShards(context.Background(), cluster, bundle)
	if err == nil || !strings.Contains(err.Error(), "spec.hieraSecrets.shards") {
		t.Fatalf("expected a size error naming spec.hieraSecrets.shards, got %v", err)
	}
	if layout.largestBytes <= maxSecretDataBytes {
		t.Errorf("layout must carry the measured size, got %d", layout.largestBytes)
	}
	if got := hieraSecretNamesIn(t, c); len(got) != 0 {
		t.Errorf("nothing may be written when a shard is over the limit, got %v", got)
	}
}

func TestHieraVolumeProjectsShards(t *testing.T) {
	v := hieraSecretsVolume([]string{"a", "b", "c"})
	if v.Projected == nil || len(v.Projected.Sources) != 3 || v.Projected.Sources[2].Secret.Name != "c" {
		t.Errorf("unexpected projected volume %+v", v)
	}
	if v := hieraSecretsVolume([]string{"x"}); v.Secret == nil || v.Secret.SecretName != "x" {
		t.Errorf("one shard must stay a plain Secret volume, got %+v", v)
	}
}

func metaCondition(typ, status, reason, msg string) metav1.Condition {
	return metav1.Condition{Type: typ, Status: metav1.ConditionStatus(status), Reason: reason, Message: msg}
}
