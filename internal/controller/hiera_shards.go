package controller

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	openvoxv1alpha1 "github.com/yboikov/voxkube-operator/api/v1alpha1"
)

const (
	// maxSecretDataBytes is Kubernetes' limit on a Secret: the API server sums the decoded value lengths
	// (ValidateSecret, core.MaxSecretSize). Base64 exists only on the wire, so raw bytes are the right measure.
	maxSecretDataBytes = 1 << 20
	// hieraWarnPercent is the fill level of the fullest shard at which HieraSecretsCapacity turns False.
	hieraWarnPercent = 80
	// ConditionHieraSecretsCapacity reports whether the rendered Hiera Secrets have room to grow.
	ConditionHieraSecretsCapacity = "HieraSecretsCapacity"
)

// hieraLayout is what reconcileHieraBridge wrote.
type hieraLayout struct {
	names        []string
	largestBytes int64
}

func (l hieraLayout) largestPercent() int32 {
	return int32(l.largestBytes * 100 / maxSecretDataBytes)
}

// hieraShardCount is spec.hieraSecrets.shards, at least 1.
func hieraShardCount(cluster *openvoxv1alpha1.VoxCluster) int {
	if n := int(cluster.Spec.HieraSecrets.Shards); n > 1 {
		return n
	}
	return 1
}

// hieraShardNames are the Secret names of the active shard set. The shard count is part of the names, so a resize
// writes a new set next to the old one instead of re-hashing files inside Secrets old compiler pods still mount.
func hieraShardNames(cluster *openvoxv1alpha1.VoxCluster) []string {
	n := hieraShardCount(cluster)
	if n == 1 {
		return []string{cluster.Name + "-hiera-secrets"}
	}
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("%s-hiera-secrets-s%d-%d", cluster.Name, n, i)
	}
	return names
}

// hieraShardIndex places a Hiera file: common.yaml in shard 0, node files by certname, role files by role name
// (SHA-256, uniform even for sequential host names).
func hieraShardIndex(file string, n int) int {
	if n <= 1 || file == "common.yaml" {
		return 0
	}
	key := strings.TrimSuffix(file, ".yaml")
	switch {
	case strings.HasPrefix(key, "nodes_"):
		key = "node/" + strings.TrimPrefix(key, "nodes_")
	case strings.HasPrefix(key, "roles_"):
		key = "role/" + strings.TrimPrefix(key, "roles_")
	}
	sum := sha256.Sum256([]byte(key))
	return int(binary.BigEndian.Uint64(sum[:8]) % uint64(n))
}

// shardHieraBundle splits the rendered files into n shards. Every file lands in exactly one shard.
func shardHieraBundle(bundle map[string][]byte, n int) []map[string][]byte {
	shards := make([]map[string][]byte, n)
	for i := range shards {
		shards[i] = map[string][]byte{}
	}
	for file, data := range bundle {
		shards[hieraShardIndex(file, n)][file] = data
	}
	return shards
}

func secretDataSize(data map[string][]byte) int64 {
	var total int64
	for _, v := range data {
		total += int64(len(v))
	}
	return total
}

// writeHieraShards writes the shard Secrets of the active set and reports their names and the fullest shard. A shard
// over the limit fails before anything is written, with a message saying what to change.
func (r *VoxClusterReconciler) writeHieraShards(ctx context.Context, cluster *openvoxv1alpha1.VoxCluster,
	bundle map[string][]byte) (hieraLayout, error) {
	names := hieraShardNames(cluster)
	shards := shardHieraBundle(bundle, len(names))
	layout := hieraLayout{names: names}
	for i, data := range shards {
		size := secretDataSize(data)
		if size > layout.largestBytes {
			layout.largestBytes = size
		}
		if size > maxSecretDataBytes {
			return layout, fmt.Errorf("hiera Secret %s would hold %d bytes, over the 1 MiB Secret limit: raise "+
				"spec.hieraSecrets.shards (now %d) or set spec.hieraSecrets.flatCertificateKeys: false", names[i], size, len(names))
		}
	}
	for i, data := range shards {
		sec := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: names[i], Namespace: cluster.Namespace, Labels: voxClusterLabels(cluster.Name, "hiera-secrets")},
			Type:       corev1.SecretTypeOpaque,
			Data:       data,
		}
		if err := controllerutil.SetControllerReference(cluster, sec, r.Scheme); err != nil {
			return layout, err
		}
		if err := r.createOrUpdateSecret(ctx, sec); err != nil {
			return layout, err
		}
	}
	return layout, nil
}

// hieraSecretsVolume mounts the shard set at one directory: a plain Secret volume for one shard (as before 0.3.0),
// a projected volume merging all shards otherwise. File names are unique across shards.
func hieraSecretsVolume(names []string) corev1.Volume {
	if len(names) == 1 {
		return corev1.Volume{Name: "hiera-secrets", VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: names[0]},
		}}
	}
	sources := make([]corev1.VolumeProjection, len(names))
	for i, n := range names {
		sources[i] = corev1.VolumeProjection{Secret: &corev1.SecretProjection{
			LocalObjectReference: corev1.LocalObjectReference{Name: n},
		}}
	}
	return corev1.Volume{Name: "hiera-secrets", VolumeSource: corev1.VolumeSource{
		Projected: &corev1.ProjectedVolumeSource{Sources: sources},
	}}
}

// pruneHieraShards deletes the cluster's Hiera Secrets that aren't in the active set (none when the bridge is
// disabled): leftovers of an earlier shard count. Called only after the compilers have rolled out.
func (r *VoxClusterReconciler) pruneHieraShards(ctx context.Context, cluster *openvoxv1alpha1.VoxCluster) error {
	active := map[string]bool{}
	if cluster.Spec.HieraSecrets.Enabled == nil || *cluster.Spec.HieraSecrets.Enabled {
		for _, n := range hieraShardNames(cluster) {
			active[n] = true
		}
	}
	list := &corev1.SecretList{}
	if err := r.List(ctx, list, client.InNamespace(cluster.Namespace),
		client.MatchingLabels(voxClusterLabels(cluster.Name, "hiera-secrets"))); err != nil {
		return err
	}
	var stale []string
	for i := range list.Items {
		s := &list.Items[i]
		if active[s.Name] || !metav1.IsControlledBy(s, cluster) {
			continue
		}
		if err := r.Delete(ctx, s); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		stale = append(stale, s.Name)
	}
	// The single-shard name also by name, in case its labels were changed by hand.
	if legacy := cluster.Name + "-hiera-secrets"; !active[legacy] {
		s := &corev1.Secret{}
		err := r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: legacy}, s)
		switch {
		case err == nil && metav1.IsControlledBy(s, cluster):
			if err := r.Delete(ctx, s); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
			stale = append(stale, legacy)
		case err != nil && !apierrors.IsNotFound(err):
			return err
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		r.event(cluster, corev1.EventTypeNormal, "HieraShardsPruned", "Deleted previous Hiera Secrets: "+strings.Join(stale, ", "))
	}
	return nil
}

// setHieraCapacity sets HieraSecretsCapacity from the fullest shard and emits a Warning event when it turns False.
// It doesn't affect the phase: the data still fits, this is the time to raise spec.hieraSecrets.shards.
func (r *VoxClusterReconciler) setHieraCapacity(cluster *openvoxv1alpha1.VoxCluster, status *openvoxv1alpha1.VoxClusterStatus,
	layout *hieraLayout, setCond func(t string, ok bool, reason, msg string)) {
	if layout == nil {
		meta.RemoveStatusCondition(&status.Conditions, ConditionHieraSecretsCapacity)
		return
	}
	pct := layout.largestPercent()
	n := len(layout.names)
	switch {
	case layout.largestBytes > maxSecretDataBytes:
		setCond(ConditionHieraSecretsCapacity, false, "OverLimit", fmt.Sprintf(
			"The fullest of %d Hiera Secret shard(s) would need %d bytes, over the 1 MiB limit; raise spec.hieraSecrets.shards", n, layout.largestBytes))
	case pct >= hieraWarnPercent:
		setCond(ConditionHieraSecretsCapacity, false, "NearLimit", fmt.Sprintf(
			"The fullest of %d Hiera Secret shard(s) is %d%% of the 1 MiB limit; raise spec.hieraSecrets.shards", n, pct))
	default:
		setCond(ConditionHieraSecretsCapacity, true, "WithinLimit", fmt.Sprintf(
			"The fullest of %d Hiera Secret shard(s) is %d%% of the 1 MiB limit", n, pct))
		return
	}
	if prev := meta.FindStatusCondition(cluster.Status.Conditions, ConditionHieraSecretsCapacity); prev == nil || prev.Status != metav1.ConditionFalse {
		c := meta.FindStatusCondition(status.Conditions, ConditionHieraSecretsCapacity)
		r.event(cluster, corev1.EventTypeWarning, "HieraSecretsNearLimit", c.Message)
	}
}
