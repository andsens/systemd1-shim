package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// K8sRestarter is the "k8s" Hook: it reacts to unit commands using the
// Kubernetes API's pod/exec subresource directly (the client-go
// equivalent of `kubectl exec`), rather than shelling out to a `kubectl`
// binary. Needs to run with in-cluster credentials (a mounted
// ServiceAccount token) and RBAC granting `create` on `pods/exec` for its
// own pod - same permission `kubectl exec` would need, just exercised
// without a subprocess.
type K8sRestarter struct {
	clientset      *kubernetes.Clientset
	restConfig     *rest.Config
	podName        string
	namespace      string
	unitContainers map[string]string
	timeout        time.Duration
}

func init() {
	registerHook("k8s", func() (Hook, error) { return NewK8sRestarter() })
}

// NewK8sRestarter reads everything it needs from the environment - see
// README.md's "Configuration (env vars)" table. It's all specific to this
// hook (which pod/namespace to exec into, how to map a unit name to a
// container), so it lives here rather than in some shared config layer
// main.go would otherwise have to know about.
func NewK8sRestarter() (*K8sRestarter, error) {
	restConfig, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("loading in-cluster kubeconfig (are you actually running in a pod, with a ServiceAccount mounted?): %w", err)
	}
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("building Kubernetes clientset: %w", err)
	}

	podName := os.Getenv("POD_NAME")
	if podName == "" {
		slog.Warn("POD_NAME is not set - wire it up via the Downward API " +
			"(fieldRef: metadata.name) in the sidecar's env, see README.md. " +
			"exec calls will fail until then.")
	}
	namespace := getenvDefault("POD_NAMESPACE", "default")

	timeout := 30 * time.Second
	if s := os.Getenv("EXEC_TIMEOUT_SECONDS"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			timeout = time.Duration(n) * time.Second
		}
	}

	unitContainers, err := loadUnitContainerMap()
	if err != nil {
		return nil, err
	}

	return &K8sRestarter{
		clientset:      clientset,
		restConfig:     restConfig,
		podName:        podName,
		namespace:      namespace,
		unitContainers: unitContainers,
		timeout:        timeout,
	}, nil
}

// loadUnitContainerMap reads UNIT_CONTAINER_MAP_FILE if set, and
// normalizes its keys to always carry the ".service" suffix so lookups
// in containerFor hit regardless of how the user wrote the mapping file.
func loadUnitContainerMap() (map[string]string, error) {
	raw := map[string]string{}
	if path := os.Getenv("UNIT_CONTAINER_MAP_FILE"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, err
		}
	}
	normalized := make(map[string]string, len(raw))
	for k, v := range raw {
		normalized[normalizeUnitName(k)] = v
	}
	return normalized, nil
}

func getenvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func (k *K8sRestarter) containerFor(unitName string) string {
	if c, ok := k.unitContainers[unitName]; ok {
		return c
	}
	return strings.TrimSuffix(unitName, ".service")
}

// execIn runs command inside the sibling container via the pods/exec
// subresource - this is exactly what `kubectl exec` does internally,
// just called directly instead of through a subprocess.
func (k *K8sRestarter) execIn(ctx context.Context, unitName string, command []string) error {
	if k.podName == "" {
		return fmt.Errorf("POD_NAME is not set - see README.md (Downward API)")
	}
	container := k.containerFor(unitName)

	req := k.clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(k.podName).
		Namespace(k.namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   command,
			Stdin:     false,
			Stdout:    true,
			Stderr:    true,
			TTY:       false,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(k.restConfig, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("building exec stream for pod=%s container=%s: %w", k.podName, container, err)
	}

	var stdout, stderr bytes.Buffer
	streamErr := executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
		Tty:    false,
	})
	if streamErr != nil {
		return fmt.Errorf("exec %v in pod=%s container=%s: %w (stdout=%q stderr=%q)",
			command, k.podName, container, streamErr, stdout.String(), stderr.String())
	}
	slog.Info("k8s exec ok",
		"pod", k.podName, "container", container, "command", command,
		"stdout", stdout.String(), "stderr", stderr.String())
	return nil
}

// killall5 (sysvinit-utils) signals every process in the target
// container other than PID 1 - see README.md for why this is the chosen
// primitive and its limitations (it's blunt if a container runs more
// than one supervised process).
func (k *K8sRestarter) Restart(unitName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), k.timeout)
	defer cancel()
	return k.execIn(ctx, unitName, []string{"/sbin/killall5", "-15"})
}

func (k *K8sRestarter) Stop(unitName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), k.timeout)
	defer cancel()
	return k.execIn(ctx, unitName, []string{"/sbin/killall5", "-15"})
}

func (k *K8sRestarter) Start(unitName string) error {
	// Kubernetes keeps containers running per their restartPolicy - there's
	// no generic "start it from outside" primitive short of the container
	// already being up. Treat as a no-op success; if the container is
	// genuinely down, the next real action (Restart/Kill) will surface a
	// failure from execIn instead of a fake success here.
	return nil
}

func (k *K8sRestarter) Kill(unitName string, signal int) error {
	ctx, cancel := context.WithTimeout(context.Background(), k.timeout)
	defer cancel()
	return k.execIn(ctx, unitName, []string{"/sbin/killall5", fmt.Sprintf("-%d", signal)})
}
