package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	openvoxv1alpha1 "github.com/yboikov/voxkube-operator/api/v1alpha1"
)

const (
	codeDir       = "/etc/puppetlabs/code"
	g10kConfigDir = "/etc/puppetlabs/g10k"
	g10kAuthDir   = "/etc/puppetlabs/g10k-auth"
	g10kSSHDir    = "/etc/puppetlabs/g10k-ssh"
	// g10k hardlinks Forge modules from its cache into the environments, so the cache must live on the same
	// volume as the code (a separate volume fails with "invalid cross-device link").
	g10kCacheDir    = codeDir + "/.g10k-cache"
	tokenExpiryAnno = "voxkube.io/expires-at"
	tokenRefreshAt  = 10 * time.Minute

	codeReasonGit      = "GitSynced"
	codeReasonGitSide  = "GitSidecar"
	codeReasonPollFail = "GitPollFailed"
	codeReasonAuthFail = "GitAuthFailed"
	codeReasonPVC      = "PersistentVolumeClaim"
	codeReasonImage    = "ImageVolume"
	codeReasonDemo     = "DemoCode"
)

// codePlan describes how compiler pods receive Puppet code.
type codePlan struct {
	volumes        []corev1.Volume
	codeVolume     corev1.Volume
	codeReadOnly   bool
	initContainers []corev1.Container
	sidecars       []corev1.Container
	podAnnotations map[string]string
	seedDemo       bool
	observedCommit string
	pollInterval   time.Duration

	ready   bool
	reason  string
	message string
}

// g10kDeployScript deploys every branch of the control-repo with g10k and optionally generates resource types.
const g10kDeployScript = `#!/bin/sh
# Rendered by voxkube-operator. Deploys the control-repo with g10k.
set -eu
export HOME="${HOME:-/tmp}"
export GIT_TERMINAL_PROMPT=0
AUTH=` + g10kAuthDir + `
SSHDIR=` + g10kSSHDir + `

if [ -s "$AUTH/token" ] || [ -s "$AUTH/password" ]; then
  export GIT_ASKPASS=` + g10kConfigDir + `/askpass.sh
fi

if [ -s "$SSHDIR/ssh-privatekey" ]; then
  mkdir -p /tmp/g10k-ssh
  chmod 700 /tmp/g10k-ssh
  cp "$SSHDIR/ssh-privatekey" /tmp/g10k-ssh/id
  chmod 600 /tmp/g10k-ssh/id
  STRICT=accept-new
  if [ -s "$SSHDIR/known_hosts" ]; then
    cp "$SSHDIR/known_hosts" /tmp/g10k-ssh/known_hosts
    STRICT=yes
  else
    touch /tmp/g10k-ssh/known_hosts
  fi
  export GIT_SSH_COMMAND="ssh -i /tmp/g10k-ssh/id -o IdentitiesOnly=yes -o StrictHostKeyChecking=$STRICT -o UserKnownHostsFile=/tmp/g10k-ssh/known_hosts"
fi

# Branch heads of the remote, hashed. The sidecar (--if-changed) deploys only when they moved since this pod's
# last deployment, or at least every VOXKUBE_FULL_SYNC_SECONDS, which also picks up Puppetfile modules that track
# a branch. The state file lives on the code volume, so the sidecar sees what g10k-init deployed.
STATE=` + codeDir + `/.voxkube-deployed-refs
refs=""
if [ -n "${VOXKUBE_GIT_REMOTE:-}" ]; then
  if out=$(git ls-remote --heads "$VOXKUBE_GIT_REMOTE"); then
    refs=$(printf '%s\n' "$out" | sha256sum | cut -d' ' -f1)
  else
    echo "WARN: git ls-remote $VOXKUBE_GIT_REMOTE failed" >&2
    [ "${1:-}" = "--if-changed" ] && exit 1
  fi
fi
if [ "${1:-}" = "--if-changed" ] && [ -n "$refs" ] && [ -s "$STATE" ]; then
  read -r last_refs last_time < "$STATE" || true
  if [ "$refs" = "${last_refs:-}" ] && [ $(( $(date +%s) - ${last_time:-0} )) -lt "${VOXKUBE_FULL_SYNC_SECONDS:-3600}" ]; then
    exit 0
  fi
fi

mkdir -p ` + codeDir + `/environments ` + g10kCacheDir + `
/usr/local/bin/g10k -config ` + g10kConfigDir + `/g10k.yaml -quiet

if [ "${VOXKUBE_GENERATE_TYPES:-true}" = "true" ]; then
  mkdir -p /tmp/puppet-gen/conf /tmp/puppet-gen/var /tmp/puppet-gen/log /tmp/puppet-gen/run
  for envdir in ` + codeDir + `/environments/*/; do
    [ -d "$envdir" ] || continue
    env=$(basename "$envdir")
    puppet generate types --environment "$env" --codedir ` + codeDir + ` \
      --confdir /tmp/puppet-gen/conf --vardir /tmp/puppet-gen/var \
      --logdir /tmp/puppet-gen/log --rundir /tmp/puppet-gen/run >/dev/null \
      || echo "WARN: puppet generate types failed for environment $env" >&2
  done
fi
[ -n "$refs" ] && echo "$refs $(date +%s)" > "$STATE"
echo "g10k deployment finished at $(date -u +%Y-%m-%dT%H:%M:%SZ)"
`

// g10kSidecarScript checks the remote every VOXKUBE_POLL_SECONDS and redeploys only when a branch moved.
const g10kSidecarScript = `#!/bin/sh
while true; do
  sleep "${VOXKUBE_POLL_SECONDS:-60}"
  ` + g10kConfigDir + `/deploy.sh --if-changed || echo "WARN: g10k deployment failed; retrying in ${VOXKUBE_POLL_SECONDS:-60}s" >&2
done
`

// g10kAskpassScript feeds HTTPS credentials to git without writing them into URLs or config files.
const g10kAskpassScript = `#!/bin/sh
AUTH=` + g10kAuthDir + `
case "$1" in
  Username*)
    if [ -s "$AUTH/username" ]; then cat "$AUTH/username"; else echo x-access-token; fi ;;
  *)
    if [ -s "$AUTH/token" ]; then cat "$AUTH/token"; else cat "$AUTH/password"; fi ;;
esac
`

func yamlSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// renderG10kConfig renders the g10k.yaml for a control-repo.
func renderG10kConfig(remote string) string {
	return fmt.Sprintf(`---
:cachedir: '%s'
use_cache_fallback: true
sources:
  control:
    remote: %s
    basedir: '%s/environments/'
    prefix: false
    invalid_branches: 'correct'
`, g10kCacheDir, yamlSingleQuote(remote), codeDir)
}

func demoPlan(reason, message string) *codePlan {
	return &codePlan{
		codeVolume:     corev1.Volume{Name: "puppet-code", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		seedDemo:       true,
		podAnnotations: map[string]string{},
		ready:          true,
		reason:         reason,
		message:        message,
	}
}

// reconcileCode decides how compilers receive Puppet code and reconciles the supporting objects.
func (r *VoxClusterReconciler) reconcileCode(ctx context.Context, cluster *openvoxv1alpha1.VoxCluster) (*codePlan, error) {
	code := cluster.Spec.Code
	mode := code.Mode
	if mode == "" {
		mode = "git"
	}

	switch mode {
	case "pvc":
		if code.ClaimName == "" {
			return demoPlan(codeReasonDemo, "spec.code.claimName is empty; serving the built-in demo environment"), nil
		}
		return &codePlan{
			codeVolume: corev1.Volume{Name: "puppet-code", VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: code.ClaimName, ReadOnly: true},
			}},
			codeReadOnly:   true,
			podAnnotations: map[string]string{},
			ready:          true,
			reason:         codeReasonPVC,
			message:        fmt.Sprintf("Puppet code mounted from PVC %s", code.ClaimName),
		}, nil
	case "imageVolume":
		if code.Image == "" {
			return demoPlan(codeReasonDemo, "spec.code.image is empty; serving the built-in demo environment"), nil
		}
		return &codePlan{
			codeVolume: corev1.Volume{Name: "puppet-code", VolumeSource: corev1.VolumeSource{
				Image: &corev1.ImageVolumeSource{Reference: code.Image, PullPolicy: corev1.PullIfNotPresent},
			}},
			codeReadOnly:   true,
			podAnnotations: map[string]string{AnnotationCodeRevision: hashBytes([]byte(code.Image))},
			ready:          true,
			reason:         codeReasonImage,
			message:        fmt.Sprintf("Puppet code mounted from image %s", code.Image),
		}, nil
	}

	if code.Git == nil || code.Git.Remote == "" {
		return demoPlan(codeReasonDemo, "spec.code.git is not set; serving the built-in demo environment"), nil
	}
	return r.reconcileGitCode(ctx, cluster)
}

func (r *VoxClusterReconciler) reconcileGitCode(ctx context.Context, cluster *openvoxv1alpha1.VoxCluster) (*codePlan, error) {
	logger := log.FromContext(ctx)
	g := cluster.Spec.Code.Git
	img := serverImage(cluster)

	poll := 60 * time.Second
	if g.PollInterval != "" {
		if d, err := time.ParseDuration(g.PollInterval); err == nil && d > 0 {
			poll = d
		}
	}
	plan := &codePlan{
		codeVolume:     corev1.Volume{Name: "puppet-code", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		podAnnotations: map[string]string{},
		pollInterval:   poll,
		ready:          true,
	}

	// 1. Credentials.
	var pollAuth *GitAuth
	authSecret := ""
	sshSecret := ""
	authType := g.AuthType
	if authType == "githubApp" && g.GitHubApp == nil {
		authType = "none"
	}
	switch authType {
	case "githubApp":
		token, secName, err := r.reconcileGitHubAppToken(ctx, cluster)
		if err != nil {
			plan.ready, plan.reason, plan.message = false, codeReasonAuthFail, "GitHub App token: "+err.Error()
		} else {
			authSecret = secName
			pollAuth = &GitAuth{Username: "x-access-token", Password: token}
		}
	case "httpsToken":
		if g.CredentialsSecretRef == "" {
			return nil, fmt.Errorf("spec.code.git.credentialsSecretRef is required for authType httpsToken")
		}
		sec := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: g.CredentialsSecretRef}, sec); err != nil {
			return nil, fmt.Errorf("reading spec.code.git.credentialsSecretRef: %w", err)
		}
		switch {
		case len(sec.Data["token"]) > 0:
			user := string(sec.Data["username"])
			if user == "" {
				user = "x-access-token"
			}
			pollAuth = &GitAuth{Username: user, Password: string(sec.Data["token"])}
		case len(sec.Data["username"]) > 0 && len(sec.Data["password"]) > 0:
			pollAuth = &GitAuth{Username: string(sec.Data["username"]), Password: string(sec.Data["password"])}
		default:
			return nil, fmt.Errorf("secret %q needs a \"token\" key or \"username\" and \"password\" keys", g.CredentialsSecretRef)
		}
		authSecret = g.CredentialsSecretRef
	case "sshKey":
		if g.CredentialsSecretRef == "" {
			return nil, fmt.Errorf("spec.code.git.credentialsSecretRef is required for authType sshKey")
		}
		sshSecret = g.CredentialsSecretRef
	}

	// 2. g10k configuration and scripts.
	cmName := fmt.Sprintf("%s-g10k", cluster.Name)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: cluster.Namespace, Labels: voxClusterLabels(cluster.Name, "g10k")},
		Data: map[string]string{
			"g10k.yaml":  renderG10kConfig(g.Remote),
			"deploy.sh":  g10kDeployScript,
			"sidecar.sh": g10kSidecarScript,
			"askpass.sh": g10kAskpassScript,
		},
	}
	if err := controllerutil.SetControllerReference(cluster, cm, r.Scheme); err != nil {
		return nil, err
	}
	if err := r.createOrUpdateConfigMap(ctx, cm); err != nil {
		return nil, err
	}
	plan.podAnnotations[AnnotationConfigHash+"-g10k"] = hashStrings(cm.Data)

	scriptMode := int32(0o755)
	plan.volumes = append(plan.volumes,
		corev1.Volume{Name: "g10k-config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: cmName}, DefaultMode: &scriptMode,
		}}},
	)
	mounts := []corev1.VolumeMount{
		{Name: "puppet-code", MountPath: codeDir},
		{Name: "g10k-config", MountPath: g10kConfigDir, ReadOnly: true},
	}
	if authSecret != "" {
		plan.volumes = append(plan.volumes, corev1.Volume{Name: "g10k-auth", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: authSecret}}})
		mounts = append(mounts, corev1.VolumeMount{Name: "g10k-auth", MountPath: g10kAuthDir, ReadOnly: true})
	}
	if sshSecret != "" {
		plan.volumes = append(plan.volumes, corev1.Volume{Name: "g10k-ssh", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: sshSecret}}})
		mounts = append(mounts, corev1.VolumeMount{Name: "g10k-ssh", MountPath: g10kSSHDir, ReadOnly: true})
	}

	generateTypes := g.GenerateTypes == nil || *g.GenerateTypes
	env := []corev1.EnvVar{
		{Name: "VOXKUBE_GENERATE_TYPES", Value: fmt.Sprintf("%t", generateTypes)},
		{Name: "VOXKUBE_POLL_SECONDS", Value: fmt.Sprintf("%d", int(poll.Seconds()))},
		{Name: "VOXKUBE_GIT_REMOTE", Value: g.Remote},
	}
	plan.initContainers = append(plan.initContainers, corev1.Container{
		Name:            "g10k-init",
		Image:           img,
		ImagePullPolicy: cluster.Spec.Server.ImagePullPolicy,
		Command:         []string{g10kConfigDir + "/deploy.sh"},
		Env:             env,
		SecurityContext: buildRestrictedContainerSecurityContext(),
		VolumeMounts:    mounts,
	})

	// 3. Rollout strategy.
	synchronized := g.SynchronizedRollout == nil || *g.SynchronizedRollout
	if synchronized && !IsHTTPRemote(g.Remote) {
		synchronized = false
		plan.message = "SSH remotes cannot be polled by the operator; using the g10k sidecar instead of synchronized rollouts. "
	}

	if !synchronized {
		plan.sidecars = append(plan.sidecars, corev1.Container{
			Name:            "g10k-sync",
			Image:           img,
			ImagePullPolicy: cluster.Spec.Server.ImagePullPolicy,
			Command:         []string{g10kConfigDir + "/sidecar.sh"},
			Env:             env,
			SecurityContext: buildRestrictedContainerSecurityContext(),
			VolumeMounts:    mounts,
		})
		if plan.ready {
			plan.reason = codeReasonGitSide
			plan.message += fmt.Sprintf("Each compiler deploys %s every %s", g.Remote, poll)
		}
		// Still report HEAD for visibility when the remote can be polled.
		if r.GitPoller != nil && IsHTTPRemote(g.Remote) {
			if rev, err := r.GitPoller.Resolve(ctx, g.Remote, pollAuth); err == nil {
				plan.observedCommit = rev.Head
			}
		}
		return plan, nil
	}

	// Synchronized: poll, and roll all compilers when any branch moves.
	prevRevision, prevCommit := r.currentCodeRevision(ctx, cluster)
	if r.GitPoller == nil {
		plan.podAnnotations[AnnotationCodeRevision] = prevRevision
		return plan, nil
	}
	rev, err := r.GitPoller.Resolve(ctx, g.Remote, pollAuth)
	if err != nil {
		logger.Info("Git poll failed; keeping the current code revision", "remote", g.Remote, "reason", err.Error())
		if prevRevision != "" {
			plan.podAnnotations[AnnotationCodeRevision] = prevRevision
		}
		if prevCommit != "" {
			plan.podAnnotations[AnnotationObservedCommit] = prevCommit
		}
		plan.ready, plan.reason, plan.message = false, codeReasonPollFail, err.Error()
		return plan, nil
	}
	plan.observedCommit = rev.Head
	plan.podAnnotations[AnnotationCodeRevision] = rev.RefsHash
	plan.podAnnotations[AnnotationObservedCommit] = rev.Head
	if plan.ready {
		plan.reason = codeReasonGit
		plan.message = fmt.Sprintf("Compilers serve %s at %s (rolled on every branch change)", g.Remote, shortSHA(rev.Head))
	}
	return plan, nil
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// currentCodeRevision reads the code revision currently rolled out on the compiler Deployment.
func (r *VoxClusterReconciler) currentCodeRevision(ctx context.Context, cluster *openvoxv1alpha1.VoxCluster) (string, string) {
	d := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: cluster.Name + "-server"}, d); err != nil {
		return "", ""
	}
	a := d.Spec.Template.Annotations
	return a[AnnotationCodeRevision], a[AnnotationObservedCommit]
}

// reconcileGitHubAppToken returns a valid installation token, minting a new one only when the cached token in
// <cluster>-g10k-git-token expires within 10 minutes.
func (r *VoxClusterReconciler) reconcileGitHubAppToken(ctx context.Context, cluster *openvoxv1alpha1.VoxCluster) (string, string, error) {
	app := cluster.Spec.Code.Git.GitHubApp
	secName := fmt.Sprintf("%s-g10k-git-token", cluster.Name)

	existing := &corev1.Secret{}
	getErr := r.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: secName}, existing)
	if getErr != nil && !apierrors.IsNotFound(getErr) {
		return "", "", getErr
	}
	found := getErr == nil
	if found && len(existing.Data["token"]) > 0 {
		if exp, perr := time.Parse(time.RFC3339, existing.Annotations[tokenExpiryAnno]); perr == nil && time.Until(exp) > tokenRefreshAt {
			return string(existing.Data["token"]), secName, nil
		}
	}

	if r.TokenExchanger == nil {
		return "", "", fmt.Errorf("no GitHub App token exchanger configured")
	}
	keySec := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: app.PrivateKeySecretRef.Name}, keySec); err != nil {
		return "", "", fmt.Errorf("reading GitHub App private key Secret %q: %w", app.PrivateKeySecretRef.Name, err)
	}
	pemBytes := keySec.Data[app.PrivateKeySecretRef.Key]
	if len(pemBytes) == 0 {
		return "", "", fmt.Errorf("secret %q has no key %q", app.PrivateKeySecretRef.Name, app.PrivateKeySecretRef.Key)
	}
	token, expiresAt, err := r.TokenExchanger.ExchangeInstallationToken(ctx, app.APIBaseURL, app.AppID, app.InstallationID, pemBytes)
	if err != nil {
		return "", "", err
	}
	if expiresAt.IsZero() {
		expiresAt = time.Now().Add(time.Hour)
	}

	tokenSec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        secName,
			Namespace:   cluster.Namespace,
			Labels:      voxClusterLabels(cluster.Name, "g10k-token"),
			Annotations: map[string]string{tokenExpiryAnno: expiresAt.UTC().Format(time.RFC3339)},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"token": []byte(token), "username": []byte("x-access-token")},
	}
	if err := controllerutil.SetControllerReference(cluster, tokenSec, r.Scheme); err != nil {
		return "", "", err
	}
	if !found {
		if err := r.Create(ctx, tokenSec); err != nil && !apierrors.IsAlreadyExists(err) {
			return "", "", err
		}
	} else {
		existing.Data = tokenSec.Data
		if existing.Annotations == nil {
			existing.Annotations = map[string]string{}
		}
		existing.Annotations[tokenExpiryAnno] = tokenSec.Annotations[tokenExpiryAnno]
		existing.Labels = tokenSec.Labels
		if err := r.Update(ctx, existing); err != nil {
			return "", "", err
		}
	}
	return token, secName, nil
}
