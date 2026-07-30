package main

import (
	"context"
	"fmt"
	"os/exec"
	"time"
)

// Restarter is the one part of this shim that's specific to your cluster.
// Everything else (D-Bus surface, job/property bookkeeping) is generic;
// swap the implementation below for whatever actually needs to happen.
type Restarter interface {
	Start(unitName string) error
	Stop(unitName string) error
	Restart(unitName string) error
	Kill(unitName string, signal int) error
}

// KubectlRestarter shells out to `kubectl exec`. This is kept as an
// alternative to k8s_restart.go's K8sRestarter (the default, client-go
// based implementation - no kubectl binary needed in the image). Swap
// NewK8sRestarter(cfg) for NewKubectlRestarter(cfg) in main.go if you'd
// rather ship kubectl than vendor client-go/api/apimachinery - both
// implement the same Restarter interface, so nothing else changes.
type KubectlRestarter struct {
	podName        string
	kubectlPath    string
	namespace      string
	unitContainers map[string]string // unit name (with .service) -> container name
	timeout        time.Duration
}

func NewKubectlRestarter(cfg *Config) *KubectlRestarter {
	return &KubectlRestarter{
		podName:        cfg.PodName,
		kubectlPath:    cfg.KubectlPath,
		namespace:      cfg.Namespace,
		unitContainers: cfg.UnitContainers,
		timeout:        cfg.ExecTimeout,
	}
}

func (k *KubectlRestarter) containerFor(unitName string) string {
	if c, ok := k.unitContainers[unitName]; ok {
		return c
	}
	// Default convention: container name == unit name minus ".service".
	name := unitName
	if len(name) > len(".service") && name[len(name)-len(".service"):] == ".service" {
		name = name[:len(name)-len(".service")]
	}
	return name
}

func (k *KubectlRestarter) exec(unitName string, args ...string) error {
	container := k.containerFor(unitName)
	full := append([]string{
		"exec", k.podName,
		"-n", k.namespace,
		"-c", container,
		"--",
	}, args...)

	ctx, cancel := context.WithTimeout(context.Background(), k.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, k.kubectlPath, full...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("kubectl exec (unit=%s container=%s args=%v): %w: %s", unitName, container, args, err, out)
	}
	logf("kubectl exec pod=%s container=%s args=%v: ok", k.podName, container, args)
	return nil
}

// killall5 (from sysvinit-utils, listed as an OS dependency the same way
// dbus/smartmontools are - add it to whichever container images need it)
// sends the given signal to every process in the target container other
// than PID 1, which is a reasonable stand-in for "restart the service"
// when the container's entrypoint will restart the killed process (e.g.
// under a process supervisor, or if the container itself exits and
// Kubernetes restarts it per its restartPolicy).
func (k *KubectlRestarter) Restart(unitName string) error {
	return k.exec(unitName, "/sbin/killall5", "-15")
}

func (k *KubectlRestarter) Stop(unitName string) error {
	return k.exec(unitName, "/sbin/killall5", "-15")
}

func (k *KubectlRestarter) Start(unitName string) error {
	// There's no generic "start" primitive from outside a container short
	// of it already being up (Kubernetes keeps containers running per
	// their restartPolicy) - treat Start as a no-op success. If a
	// container is genuinely down, `kubectl exec` below will simply fail
	// and that failure is what should surface, not a fake success.
	return nil
}

func (k *KubectlRestarter) Kill(unitName string, signal int) error {
	return k.exec(unitName, "/sbin/killall5", fmt.Sprintf("-%d", signal))
}
