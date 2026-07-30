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
	"strings"
	"syscall"

	"github.com/docopt/docopt-go"
	"github.com/godbus/dbus/v5"
)

const busName = "org.freedesktop.systemd1"

func logf(format string, args ...interface{}) {
	log.Printf("[systemd1-shim] "+format, args...)
}

// usageDoc is both the --help text and the docopt grammar it's parsed
// against - the two can't drift apart. %s is filled in with the
// currently registered hook names (hook.go) so --help always lists
// exactly what --hook will actually accept.
const usageDoc = `systemd1-shim - fake org.freedesktop.systemd1 D-Bus service

Usage:
  systemd1-shim [--hook=<name>]
  systemd1-shim -h | --help

Options:
  -h --help      Show this help.
  --hook=<name>  Which hook to load for reacting to unit start/stop/restart/
                 kill commands [default: k8s]. Available: %s.
`

func main() {
	var cliArgs struct {
		Hook string `docopt:"--hook"`
	}
	usage := fmt.Sprintf(usageDoc, strings.Join(hookNames(), ", "))
	opts, err := docopt.ParseArgs(usage, os.Args[1:], "")
	if err != nil {
		fmt.Fprintln(os.Stderr, "systemd1-shim: parsing arguments:", err)
		os.Exit(1)
	}
	if err := opts.Bind(&cliArgs); err != nil {
		fmt.Fprintln(os.Stderr, "systemd1-shim: binding arguments:", err)
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
	hook, err := loadHook(cliArgs.Hook)
	if err != nil {
		fmt.Fprintln(os.Stderr, "systemd1-shim: loading hook:", err)
		os.Exit(1)
	}

	manager, err := newManager(conn, units, hook)
	if err != nil {
		fmt.Fprintln(os.Stderr, "systemd1-shim: exporting Manager:", err)
		os.Exit(1)
	}
	if err := conn.Export(manager, managerPath, managerIface); err != nil {
		fmt.Fprintln(os.Stderr, "systemd1-shim: exporting Manager methods:", err)
		os.Exit(1)
	}

	logf("claimed %s, hook=%s", busName, cliArgs.Hook)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	logf("shutting down")
}
