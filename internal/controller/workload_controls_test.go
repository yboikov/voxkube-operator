package controller

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	openvoxv1alpha1 "github.com/yboikov/voxkube-operator/api/v1alpha1"
)

func controlsCluster() *openvoxv1alpha1.VoxCluster {
	c := pkiCluster()
	two := int32(2)
	c.Spec.Server.Replicas = &two
	c.Spec.CA.JavaArgs = "-Xms512m -Xmx512m"
	c.Spec.CA.Resources = corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("768Mi")}}
	c.Spec.Database.JavaArgs = "-Xms1g -Xmx1g"
	c.Spec.Database.Resources = corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")}}
	c.Spec.Database.SoftWriteFailure = true
	c.Spec.Placement = openvoxv1alpha1.PodPlacementSpec{
		NodeSelector:     map[string]string{"node-role.kubernetes.io/infra": ""},
		Tolerations:      []corev1.Toleration{{Key: "node-role.kubernetes.io/infra", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}},
		ImagePullSecrets: []corev1.LocalObjectReference{{Name: "mirror"}},
	}
	return c
}

func getObj(t *testing.T, c client.Client, name string, obj client.Object) {
	t.Helper()
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ovx", Name: name}, obj); err != nil {
		t.Fatalf("%T %s: %v", obj, name, err)
	}
}

func envValue(c corev1.Container, name string) string {
	for _, e := range c.Env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

func TestProductionWorkloadControls(t *testing.T) {
	s := newVoxKubeScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(controlsCluster()).WithStatusSubresource(&openvoxv1alpha1.VoxCluster{}).Build()
	r := &VoxClusterReconciler{Client: c, Scheme: s, GitPoller: &mockGitPoller{sha: "x"}}
	reconcilePKICluster(t, c, r)

	ca := &appsv1.StatefulSet{}
	getObj(t, c, "prod-ca", ca)
	srv := &appsv1.Deployment{}
	getObj(t, c, "prod-server", srv)
	db := &appsv1.Deployment{}
	getObj(t, c, "prod-db", db)

	caC, srvC, dbC := ca.Spec.Template.Spec.Containers[0], srv.Spec.Template.Spec.Containers[0], db.Spec.Template.Spec.Containers[0]
	if envValue(caC, "JAVA_ARGS") != "-Xms512m -Xmx512m" || caC.Resources.Requests.Memory().String() != "768Mi" {
		t.Errorf("CA javaArgs/resources not applied: %v %v", caC.Env, caC.Resources)
	}
	if envValue(dbC, "JAVA_ARGS") != "-Xms1g -Xmx1g" || dbC.Resources.Requests.Cpu().String() != "500m" {
		t.Errorf("DB javaArgs/resources not applied: %v %v", dbC.Env, dbC.Resources)
	}
	for name, ct := range map[string]corev1.Container{"ca": caC, "server": srvC, "db": dbC} {
		if ct.StartupProbe == nil || ct.LivenessProbe == nil || ct.ReadinessProbe == nil {
			t.Errorf("%s: expected startup, liveness and readiness probes", name)
			continue
		}
		if budget := ct.StartupProbe.PeriodSeconds * ct.StartupProbe.FailureThreshold; budget < 300 {
			t.Errorf("%s: startup budget %ds is too short for a cold JVM", name, budget)
		}
		if grace := ct.LivenessProbe.PeriodSeconds * ct.LivenessProbe.FailureThreshold; grace < 120 {
			t.Errorf("%s: liveness must tolerate at least 2 minutes of failures, got %ds", name, grace)
		}
	}

	for name, spec := range map[string]corev1.PodSpec{"ca": ca.Spec.Template.Spec, "server": srv.Spec.Template.Spec, "db": db.Spec.Template.Spec} {
		if _, ok := spec.NodeSelector["node-role.kubernetes.io/infra"]; !ok || len(spec.Tolerations) != 1 ||
			len(spec.ImagePullSecrets) != 1 || spec.ImagePullSecrets[0].Name != "mirror" {
			t.Errorf("%s: placement not applied: %+v", name, spec)
		}
	}
	if a := srv.Spec.Template.Spec.Affinity; a == nil || a.PodAntiAffinity == nil ||
		len(a.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution) != 1 {
		t.Error("compilers must prefer different nodes by default")
	}
	if db.Spec.Template.Spec.Affinity != nil {
		t.Error("only compilers get a default affinity")
	}

	cm := &corev1.ConfigMap{}
	getObj(t, c, "prod-server-config", cm)
	found := false
	for _, v := range cm.Data {
		if strings.Contains(v, "soft_write_failure = true") {
			found = true
		}
	}
	if !found {
		t.Error("softWriteFailure must reach puppetdb.conf")
	}

	// Two compilers: PDB; one OpenVox DB replica: none (it would block drains).
	pdb := &policyv1.PodDisruptionBudget{}
	getObj(t, c, "prod-server", pdb)
	if pdb.Spec.MaxUnavailable == nil || pdb.Spec.MaxUnavailable.IntValue() != 1 {
		t.Errorf("expected maxUnavailable 1, got %+v", pdb.Spec)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ovx", Name: "prod-db"}, &policyv1.PodDisruptionBudget{}); !apierrors.IsNotFound(err) {
		t.Errorf("a single OpenVox DB replica must not get a PDB, got %v", err)
	}
}

func TestUserAffinityAndPDBToggle(t *testing.T) {
	ctx := context.Background()
	s := newVoxKubeScheme(t)
	cluster := controlsCluster()
	cluster.Spec.Placement.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cluster).WithStatusSubresource(&openvoxv1alpha1.VoxCluster{}).Build()
	r := &VoxClusterReconciler{Client: c, Scheme: s, GitPoller: &mockGitPoller{sha: "x"}}
	reconcilePKICluster(t, c, r)

	srv := &appsv1.Deployment{}
	getObj(t, c, "prod-server", srv)
	if a := srv.Spec.Template.Spec.Affinity; a == nil || a.NodeAffinity == nil || a.PodAntiAffinity != nil {
		t.Errorf("a user affinity replaces the default, got %+v", a)
	}

	// Switching the PDB off deletes it.
	live := &openvoxv1alpha1.VoxCluster{}
	getObj(t, c, "prod", live)
	off := false
	live.Spec.Server.PodDisruptionBudget = &openvoxv1alpha1.PodDisruptionBudgetSpec{Enabled: &off}
	if err := c.Update(ctx, live); err != nil {
		t.Fatal(err)
	}
	reconcilePKICluster(t, c, r)
	if err := c.Get(ctx, types.NamespacedName{Namespace: "ovx", Name: "prod-server"}, &policyv1.PodDisruptionBudget{}); !apierrors.IsNotFound(err) {
		t.Errorf("a disabled PDB must be deleted, got %v", err)
	}

	// Back on, then down to one replica: deleted again.
	getObj(t, c, "prod", live)
	live.Spec.Server.PodDisruptionBudget = nil
	one := int32(1)
	live.Spec.Server.Replicas = &one
	if err := c.Update(ctx, live); err != nil {
		t.Fatal(err)
	}
	reconcilePKICluster(t, c, r)
	if err := c.Get(ctx, types.NamespacedName{Namespace: "ovx", Name: "prod-server"}, &policyv1.PodDisruptionBudget{}); !apierrors.IsNotFound(err) {
		t.Errorf("one compiler must not have a PDB, got %v", err)
	}
}
