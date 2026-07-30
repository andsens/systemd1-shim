package hooks

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestContainerFor(t *testing.T) {
	k := &K8sRestarter{}

	t.Run("falls back to unit-name-minus-.service when unset", func(t *testing.T) {
		if got := k.containerFor("unmapped.service"); got != "unmapped" {
			t.Errorf("containerFor(%q) = %q, want %q", "unmapped.service", got, "unmapped")
		}
	})

	t.Run("SD1_SHIM_K8S_MAP_<UNIT_NAME> overrides it", func(t *testing.T) {
		t.Setenv("SD1_SHIM_K8S_MAP_MY_APP", "custom-container")
		if got := k.containerFor("my-app.service"); got != "custom-container" {
			t.Errorf("containerFor(%q) = %q, want %q", "my-app.service", got, "custom-container")
		}
	})
}

func TestK8sRestarterStartIsANoop(t *testing.T) {
	k := &K8sRestarter{}
	if err := k.Start("anything.service"); err != nil {
		t.Errorf("Start() = %v, want nil - Kubernetes keeps containers running per restartPolicy, nothing to do", err)
	}
}

func TestPidForContainer(t *testing.T) {
	writeCgroup := func(t *testing.T, procRoot, pid, content string) {
		t.Helper()
		dir := filepath.Join(procRoot, pid)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "cgroup"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("finds the matching PID, ignoring non-PID entries", func(t *testing.T) {
		root := t.TempDir()
		writeCgroup(t, root, "100", "0::/kubepods/pod-x/cri-containerd-abc123.scope")
		if err := os.MkdirAll(filepath.Join(root, "self"), 0o755); err != nil {
			t.Fatal(err) // "self" is a real /proc entry that isn't a PID - must be skipped, not error
		}

		pid, err := pidForContainer(root, "abc123")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if pid != 100 {
			t.Errorf("pid = %d, want 100", pid)
		}
	})

	t.Run("multiple matches - lowest PID (the entrypoint) wins", func(t *testing.T) {
		root := t.TempDir()
		writeCgroup(t, root, "200", "0::/kubepods/pod-x/cri-containerd-target.scope")
		writeCgroup(t, root, "150", "0::/kubepods/pod-x/cri-containerd-target.scope") // a child, spawned later, but written first here
		writeCgroup(t, root, "999", "0::/kubepods/pod-x/cri-containerd-other.scope")  // different container - must not match

		pid, err := pidForContainer(root, "target")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if pid != 150 {
			t.Errorf("pid = %d, want 150 (the lowest matching PID)", pid)
		}
	})

	t.Run("no match errors", func(t *testing.T) {
		root := t.TempDir()
		writeCgroup(t, root, "1", "0::/kubepods/pod-x/cri-containerd-someone-else.scope")

		if _, err := pidForContainer(root, "not-there"); err == nil {
			t.Fatal("expected an error when no process matches, got nil")
		}
	})

	t.Run("a PID dir with no readable cgroup file is skipped, not fatal", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "300"), 0o755); err != nil {
			t.Fatal(err) // no cgroup file inside - simulates a process that exited mid-scan
		}
		writeCgroup(t, root, "301", "0::/kubepods/pod-x/cri-containerd-findme.scope")

		pid, err := pidForContainer(root, "findme")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if pid != 301 {
			t.Errorf("pid = %d, want 301", pid)
		}
	})
}

func TestContainerIDFor(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "my-pod", Namespace: "my-ns"},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "my-app", ContainerID: "containerd://abc123"},
				{Name: "not-started-yet", ContainerID: ""},
			},
		},
	}
	k := &K8sRestarter{
		clientset: fake.NewSimpleClientset(pod),
		podName:   "my-pod",
		namespace: "my-ns",
	}

	t.Run("found, runtime prefix stripped", func(t *testing.T) {
		id, err := k.containerIDFor(context.Background(), "my-app")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id != "abc123" {
			t.Errorf("containerID = %q, want %q", id, "abc123")
		}
	})

	t.Run("container not started yet", func(t *testing.T) {
		if _, err := k.containerIDFor(context.Background(), "not-started-yet"); err == nil {
			t.Fatal("expected an error for a container with no ContainerID yet, got nil")
		}
	})

	t.Run("unknown container name", func(t *testing.T) {
		if _, err := k.containerIDFor(context.Background(), "nope"); err == nil {
			t.Fatal("expected an error for an unknown container name, got nil")
		}
	})
}

// TestNewK8sRestarter_OutsideCluster confirms the failure mode when run
// where this hook is actually meant to run from: NOT in a cluster (e.g.
// this test suite, or CI). If KUBERNETES_SERVICE_HOST is set, we're
// unexpectedly inside a real (or fake) cluster and can't exercise this
// path, so skip rather than assert something we can't control.
func TestNewK8sRestarter_OutsideCluster(t *testing.T) {
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		t.Skip("running inside a cluster - can't test the not-in-a-cluster error path here")
	}
	_, err := NewK8sRestarter()
	if err == nil {
		t.Fatal("expected an error building a K8sRestarter outside a cluster, got nil")
	}
}
