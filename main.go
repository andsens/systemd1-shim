// systemd1-shim fakes just enough of org.freedesktop.systemd1 for both
// `systemctl` and other D-Bus clients to work without a real systemd
// running as PID 1. See README.md for the full D-Bus surface covered.
package main

import (
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/docopt/docopt-go"
	"github.com/godbus/dbus/v5"

	"github.com/andsens/systemd1-shim/hooks"
)

const busName = "org.freedesktop.systemd1"

func main() {
	usage := fmt.Sprintf(`systemd1-shim - fake org.freedesktop.systemd1 D-Bus service

Usage:
  systemd1-shim [--hook=<name>]
  systemd1-shim -h | --help

Options:
  -h --help      Show this help.
  --hook=<name>  Which hook to load for reacting to unit start/stop/restart/
                 kill commands [default: noop]. Available: %s.
`, strings.Join(hooks.Names(), ", "))
	var cliArgs struct {
		Hook string `docopt:"--hook"`
	}
	opts, err := docopt.ParseDoc(usage)
	if err != nil {
		slog.Error("parsing arguments", "error", err)
		os.Exit(1)
	}
	if err := opts.Bind(&cliArgs); err != nil {
		slog.Error("binding arguments", "error", err)
		os.Exit(1)
	}

	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		slog.Error("connecting to system bus", "error", err)
		os.Exit(1)
	}
	defer conn.Close()

	reply, err := conn.RequestName(busName, dbus.NameFlagDoNotQueue)
	if err != nil {
		slog.Error("requesting bus name", "error", err)
		os.Exit(1)
	}
	if reply != dbus.RequestNameReplyPrimaryOwner {
		slog.Error("could not become primary owner of bus name - is a real systemd "+
			"(or another instance of this shim) already on this bus?",
			"bus", busName, "reply", reply)
		os.Exit(1)
	}

	hook, err := hooks.Load(cliArgs.Hook)
	if err != nil {
		slog.Error("loading hook", "error", err)
		os.Exit(1)
	}

	manager, err := newManager(conn, hook)
	if err != nil {
		slog.Error("exporting Manager", "error", err)
		os.Exit(1)
	}
	if err := conn.Export(manager, managerPath, managerIface); err != nil {
		slog.Error("exporting Manager methods", "error", err)
		os.Exit(1)
	}

	slog.Info("claimed bus name", "bus", busName, "hook", cliArgs.Hook)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	slog.Info("shutting down")
}
