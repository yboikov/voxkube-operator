package controller

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	openvoxv1alpha1 "github.com/yboikov/voxkube-operator/api/v1alpha1"
)

func revWith(branches map[string]string) GitRevision {
	var heads []string
	for b, sha := range branches {
		heads = append(heads, "refs/heads/"+b+" "+sha)
	}
	return GitRevision{Head: branches["production"], RefsHash: hashHeads(heads), Branches: branches}
}

func TestSelectBranches(t *testing.T) {
	sha := func(c string) string { return strings.Repeat(c, 40) }
	base := map[string]string{"production": sha("a"), "staging": sha("b"), "feat/x": sha("c")}
	all := revWith(base)

	if got, _ := SelectBranches(all, nil); got.RefsHash != all.RefsHash {
		t.Error("without a filter the revision must be unchanged (no rollout on upgrade)")
	}
	sel, err := SelectBranches(all, []string{"staging", "production"})
	if err != nil {
		t.Fatal(err)
	}
	if sel.Head != sha("b") {
		t.Errorf("Head must be the first configured branch that exists (staging), got %s", sel.Head)
	}
	moved := map[string]string{"production": sha("a"), "staging": sha("b"), "feat/x": sha("d")}
	sel2, _ := SelectBranches(revWith(moved), []string{"staging", "production"})
	if sel2.RefsHash != sel.RefsHash {
		t.Error("a push to an unselected branch must not change RefsHash")
	}
	moved["production"] = sha("e")
	sel3, _ := SelectBranches(revWith(moved), []string{"staging", "production"})
	if sel3.RefsHash == sel.RefsHash {
		t.Error("a push to a selected branch must change RefsHash")
	}
	if _, err := SelectBranches(all, []string{"nope"}); err == nil {
		t.Error("no matching branch must be an error")
	}
	if got, _ := SelectBranches(all, []string{"missing", "staging"}); got.Head != sha("b") {
		t.Errorf("missing branches are skipped when another exists, got %s", got.Head)
	}
}

func TestParseRefAdvertisementBranches(t *testing.T) {
	pkt := func(s string) string { return fmt.Sprintf("%04x", len(s)+4) + s }
	body := pkt("# service=git-upload-pack\n") + "0000" +
		pkt(strings.Repeat("a", 40)+" HEAD\x00multi_ack\n") +
		pkt(strings.Repeat("a", 40)+" refs/heads/production\n") +
		pkt(strings.Repeat("b", 40)+" refs/heads/feat/x\n") +
		pkt(strings.Repeat("c", 40)+" refs/tags/v1\n") + "0000"
	rev, err := ParseRefAdvertisement([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(rev.Branches) != 2 || rev.Branches["feat/x"] != strings.Repeat("b", 40) {
		t.Errorf("unexpected branches %v", rev.Branches)
	}
}

func TestG10kConfigBranchFilter(t *testing.T) {
	if strings.Contains(renderG10kConfig("https://x/r.git", nil), "filter_regex") {
		t.Error("no branches, no filter")
	}
	cfg := renderG10kConfig("https://x/r.git", []string{"production", "release/1.0", "a+b"})
	if !strings.Contains(cfg, `    filter_regex: '^(production|release/1\.0|a\+b)$'`) {
		t.Errorf("expected an anchored, escaped filter_regex, got:\n%s", cfg)
	}
}

func TestSynchronizedRolloutIgnoresUnselectedBranches(t *testing.T) {
	ctx := context.Background()
	s := newVoxKubeScheme(t)
	sync := true
	cluster := &openvoxv1alpha1.VoxCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "prod", Namespace: "ovx", UID: "u1"},
		Spec: openvoxv1alpha1.VoxClusterSpec{Code: openvoxv1alpha1.ClusterCodeSpec{Mode: "git", Git: &openvoxv1alpha1.GitCodeSpec{
			Remote: "https://git.example.com/control.git", AuthType: "none", SynchronizedRollout: &sync,
			Branches: []string{"staging"},
		}}},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cluster).Build()
	poller := &branchPoller{branches: map[string]string{"production": strings.Repeat("a", 40), "staging": strings.Repeat("b", 40)}}
	r := &VoxClusterReconciler{Client: c, Scheme: s, GitPoller: poller}

	plan, err := r.reconcileCode(ctx, cluster)
	if err != nil {
		t.Fatal(err)
	}
	if plan.observedCommit != strings.Repeat("b", 40) {
		t.Errorf("observedCommit must follow the selected branch, got %s", plan.observedCommit)
	}
	first := plan.podAnnotations[AnnotationCodeRevision]

	poller.branches["production"] = strings.Repeat("c", 40)
	plan, _ = r.reconcileCode(ctx, cluster)
	if plan.podAnnotations[AnnotationCodeRevision] != first {
		t.Error("a push to an unselected branch must not roll the compilers")
	}

	// The branch list reaches g10k and deploy.sh.
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "ovx", Name: "prod-g10k"}, cm); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cm.Data["g10k.yaml"], "filter_regex: '^(staging)$'") {
		t.Errorf("g10k.yaml lacks the filter:\n%s", cm.Data["g10k.yaml"])
	}
	found := false
	for _, e := range plan.initContainers[0].Env {
		if e.Name == "VOXKUBE_GIT_BRANCHES" && e.Value == "staging" {
			found = true
		}
	}
	if !found {
		t.Error("g10k-init must get VOXKUBE_GIT_BRANCHES")
	}

	poller.branches = map[string]string{"production": strings.Repeat("a", 40)}
	plan, _ = r.reconcileCode(ctx, cluster)
	if plan.ready || plan.reason != codeReasonPollFail {
		t.Errorf("a missing configured branch must report %s, got ready=%v %s", codeReasonPollFail, plan.ready, plan.reason)
	}
}

type branchPoller struct{ branches map[string]string }

func (p *branchPoller) Resolve(context.Context, string, *GitAuth) (GitRevision, error) {
	b := map[string]string{}
	for k, v := range p.branches {
		b[k] = v
	}
	return revWith(b), nil
}

// TestDeployScriptBranchFilter runs the rendered deploy.sh with a stub git and g10k.
func TestDeployScriptBranchFilter(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	for _, tool := range []string{"sha256sum", "cut"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("no " + tool)
		}
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(g10kDeployScript, "/usr/local/bin/g10k", filepath.Join(bin, "g10k"))
	script = strings.ReplaceAll(script, "/etc/puppetlabs", dir)
	if err := os.WriteFile(filepath.Join(dir, "deploy.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// git: record the arguments, print $STUB_REFS.
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\necho \"$@\" > \"$STUB_ARGS\"\nprintf '%s' \"$STUB_REFS\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// g10k: create one environment.
	if err := os.WriteFile(filepath.Join(bin, "g10k"), []byte("#!/bin/sh\nmkdir -p \""+dir+"/code/environments/staging\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(branches, refs string) (string, error) {
		args := filepath.Join(dir, "args")
		cmd := exec.Command("sh", filepath.Join(dir, "deploy.sh"))
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "HOME="+dir,
			"VOXKUBE_GIT_REMOTE=https://git.example.com/r.git", "VOXKUBE_GIT_BRANCHES="+branches,
			"VOXKUBE_GENERATE_TYPES=false", "STUB_ARGS="+args, "STUB_REFS="+refs)
		out, err := cmd.CombinedOutput()
		a, _ := os.ReadFile(args)
		if err != nil {
			err = fmt.Errorf("%w: %s", err, out)
		}
		return strings.TrimSpace(string(a)), err
	}

	args, err := run("staging release/1.0", strings.Repeat("b", 40)+"\trefs/heads/staging\n")
	if err != nil {
		t.Fatalf("deploy.sh failed: %v", err)
	}
	if args != "ls-remote --heads https://git.example.com/r.git refs/heads/staging refs/heads/release/1.0" {
		t.Errorf("ls-remote must query only the selected branches, got %q", args)
	}
	if args, _ = run("", strings.Repeat("b", 40)+"\trefs/heads/staging\n"); args != "ls-remote --heads https://git.example.com/r.git" {
		t.Errorf("without a filter every branch is queried, got %q", args)
	}
	if _, err := run("missing", ""); err == nil {
		t.Error("deploy.sh must fail when none of the branches exist")
	}
}
