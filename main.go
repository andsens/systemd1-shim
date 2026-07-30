// systemd1-shim fakes just enough of org.freedesktop.systemd1 for both
// unifi-core's own D-Bus client code AND the real `systemctl` binary
// (already present in the unifi-core image) to work without a real
// systemd running as PID 1 anywhere in the pod.
//
// See README.md for the sidecar spec, RBAC, and the exact D-Bus/systemctl
// surface this covers.
package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/godbus/dbus/v5"
)

const busName = "org.freedesktop.systemd1"

func logf(format string, args ...interface{}) {
	log.Printf("[systemd1-shim] "+format, args...)
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "systemd1-shim: loading config:", err)
		os.Exit(1)
	}

	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		fmt.Fprintln(os.Stderr, "systemd1-shim: connecting to system bus:", err)
		os.Exit(1)
	}
	defer conn.Close()

	reply, err := conn.RequestName(busName, dbus.NameFlagDoNotQueue)
	if err != nil {
		fmt.Fprintln(os.Stderr, "systemd1-shim: requesting bus name:", err)
		os.Exit(1)
	}
	if reply != dbus.RequestNameReplyPrimaryOwner {
		fmt.Fprintf(os.Stderr,
			"systemd1-shim: could not become primary owner of %s (reply=%d) - "+
				"is a real systemd (or another instance of this shim) already on this bus?\n",
			busName, reply)
		os.Exit(1)
	}

	units := newUnitRegistry(conn)
	restarter, err := NewK8sRestarter(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "systemd1-shim: setting up Kubernetes client:", err)
		os.Exit(1)
	}

	manager, err := newManager(conn, units, restarter)
	if err != nil {
		fmt.Fprintln(os.Stderr, "systemd1-shim: exporting Manager:", err)
		os.Exit(1)
	}
	if err := conn.Export(manager, managerPath, managerIface); err != nil {
		fmt.Fprintln(os.Stderr, "systemd1-shim: exporting Manager methods:", err)
		os.Exit(1)
	}

	logf("claimed %s, pod=%s namespace=%s, %d unit->container overrides loaded",
		busName, cfg.PodName, cfg.Namespace, len(cfg.UnitContainers))

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	logf("shutting down")
}
