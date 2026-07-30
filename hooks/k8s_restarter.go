package hooks

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// K8sRestarter is the "k8s" Hook: it signals a sibling container's main
// process directly, by PID. Requires shareProcessNamespace: true on the
// pod, and RBAC granting `get` on `pods` (to read ContainerStatuses).
type K8sRestarter struct {
	clientset kubernetes.Interface
	podName   string
	namespace string
}

// apiCallTimeout bounds the one Kubernetes API call this hook makes -
// not worth making configurable for a single read.
const apiCallTimeout = 30 * time.Second

func init() {
	register("k8s", func() (Hook, error) { return NewK8sRestarter() })
}

// NewK8sRestarter reads its config from env vars - see README.md.
func NewK8sRestarter() (*K8sRestarter, error) {
	podName := os.Getenv("POD_NAME")
	if podName == "" {
		slog.Warn("POD_NAME is not set - wire it up via the Downward API " +
			"(fieldRef: metadata.name) in the sidecar's env, see README.md. " +
			"restart/stop/kill calls will fail until then.")
	}
	namespace := os.Getenv("POD_NAMESPACE")
	if namespace == "" {
		namespace = "default"
	}

	restConfig, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("loading in-cluster kubeconfig (are you actually running in a pod, with a ServiceAccount mounted?): %w", err)
	}
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("building Kubernetes clientset: %w", err)
	}

	return &K8sRestarter{
		clientset: clientset,
		podName:   podName,
		namespace: namespace,
	}, nil
}

const unitContainerEnvPrefix = "SD1_SHIM_K8S_MAP_"

// containerFor maps a unit name to a container name via
// SD1_SHIM_K8S_MAP_<UNIT_NAME> (suffix stripped, uppercased, '-' turned
// into '_'):
//
//	env:
//	  - name: SD1_SHIM_K8S_MAP_MY_APP
//	    value: my-app-container
//
// Falls back to the unit name itself (suffix stripped) if that's unset.
func (k *K8sRestarter) containerFor(unitName string) string {
	base := strings.TrimSuffix(unitName, ".service")
	envName := unitContainerEnvPrefix + strings.ToUpper(strings.ReplaceAll(base, "-", "_"))
	if c := os.Getenv(envName); c != "" {
		return c
	}
	return base
}

func (k *K8sRestarter) containerIDFor(ctx context.Context, containerName string) (string, error) {
	pod, err := k.clientset.CoreV1().Pods(k.namespace).Get(ctx, k.podName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("getting pod %s/%s: %w", k.namespace, k.podName, err)
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != containerName {
			continue
		}
		if cs.ContainerID == "" {
			return "", fmt.Errorf("container %q has no ContainerID yet (not started?)", containerName)
		}
		if i := strings.Index(cs.ContainerID, "://"); i != -1 {
			return cs.ContainerID[i+3:], nil
		}
		return cs.ContainerID, nil
	}
	return "", fmt.Errorf("no container named %q in pod %s/%s", containerName, k.namespace, k.podName)
}

// pidForContainer correlates a container ID to a PID via /proc/*/cgroup
// - Kubernetes doesn't expose this directly. Among matches, the lowest
// PID is the entrypoint; the rest are children spawned later.
func pidForContainer(procRoot, containerID string) (int, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", procRoot, err)
	}

	best := 0
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue // not a PID directory (e.g. "self", "meminfo")
		}
		cgroup, err := os.ReadFile(filepath.Join(procRoot, entry.Name(), "cgroup"))
		if err != nil {
			continue // process exited mid-scan, or unreadable - skip it
		}
		if !strings.Contains(string(cgroup), containerID) {
			continue
		}
		if best == 0 || pid < best {
			best = pid
		}
	}
	if best == 0 {
		return 0, fmt.Errorf("no process found for container %s - is shareProcessNamespace: true set on the pod?", containerID)
	}
	return best, nil
}

func (k *K8sRestarter) signalContainer(ctx context.Context, unitName string, sig syscall.Signal) error {
	if k.podName == "" {
		return fmt.Errorf("POD_NAME is not set - see README.md (Downward API)")
	}
	container := k.containerFor(unitName)

	containerID, err := k.containerIDFor(ctx, container)
	if err != nil {
		return err
	}
	pid, err := pidForContainer("/proc", containerID)
	if err != nil {
		return fmt.Errorf("finding PID for container %s: %w", container, err)
	}
	if err := syscall.Kill(pid, sig); err != nil {
		return fmt.Errorf("signaling pid %d (container %s) with %v: %w", pid, container, sig, err)
	}
	slog.Info("signaled container", "container", container, "pid", pid, "signal", sig)
	return nil
}

func (k *K8sRestarter) Restart(unitName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), apiCallTimeout)
	defer cancel()
	return k.signalContainer(ctx, unitName, syscall.SIGTERM)
}

func (k *K8sRestarter) Stop(unitName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), apiCallTimeout)
	defer cancel()
	return k.signalContainer(ctx, unitName, syscall.SIGTERM)
}

func (k *K8sRestarter) Start(unitName string) error {
	// No generic way to start a container from outside - Kubernetes
	// already keeps it running per restartPolicy. If it's actually down,
	// the next Restart/Kill call surfaces that failure instead of a fake
	// success here.
	return nil
}

func (k *K8sRestarter) Kill(unitName string, signal int) error {
	ctx, cancel := context.WithTimeout(context.Background(), apiCallTimeout)
	defer cancel()
	return k.signalContainer(ctx, unitName, syscall.Signal(signal))
}
