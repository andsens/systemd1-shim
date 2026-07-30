package main

import (
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
	"github.com/godbus/dbus/v5/prop"

	systemddbus "github.com/coreos/go-systemd/v22/dbus"
)

const (
	unitIface    = "org.freedesktop.systemd1.Unit"
	serviceIface = "org.freedesktop.systemd1.Service"
	managerIface = "org.freedesktop.systemd1.Manager"
)

// unitPath turns "foo.service" (or "foo") into a D-Bus object path
// segment, using systemd's own escaping algorithm (PathBusEscape - the
// same one kubelet uses when it talks to systemd over D-Bus), so this
// stays byte-compatible with real systemd rather than just legal.
func unitPath(name string) dbus.ObjectPath {
	base := strings.TrimSuffix(name, ".service")
	return dbus.ObjectPath("/org/freedesktop/systemd1/unit/" + systemddbus.PathBusEscape(base))
}

// Unit is one faked systemd unit, exported as a D-Bus object at
// unitPath(name).
type Unit struct {
	Name   string
	Path   dbus.ObjectPath
	conn   *dbus.Conn
	props  *prop.Properties
	masked bool
}

func newUnit(conn *dbus.Conn, name string) (*Unit, error) {
	if !strings.HasSuffix(name, ".service") {
		name += ".service"
	}
	path := unitPath(name)

	propsSpec := prop.Map{
		unitIface: {
			"Id":            {Value: name, Writable: false, Emit: prop.EmitFalse, Callback: nil},
			"LoadState":     {Value: "loaded", Writable: false, Emit: prop.EmitTrue, Callback: nil},
			"ActiveState":   {Value: "active", Writable: false, Emit: prop.EmitTrue, Callback: nil},
			"SubState":      {Value: "running", Writable: false, Emit: prop.EmitTrue, Callback: nil},
			"Description":   {Value: "Auto-created unit " + name, Writable: false, Emit: prop.EmitTrue, Callback: nil},
			"UnitFileState": {Value: "enabled", Writable: false, Emit: prop.EmitTrue, Callback: nil},
			"Following":     {Value: "", Writable: false, Emit: prop.EmitFalse, Callback: nil},
			"LoadError":     {Value: "", Writable: false, Emit: prop.EmitTrue, Callback: nil},
		},
		serviceIface: {
			"StatusText":    {Value: "", Writable: false, Emit: prop.EmitTrue, Callback: nil},
			"MainPID":       {Value: uint32(0), Writable: false, Emit: prop.EmitTrue, Callback: nil},
			"MemoryCurrent": {Value: uint64(0), Writable: false, Emit: prop.EmitTrue, Callback: nil},
			"CPUUsageNSec":  {Value: uint64(0), Writable: false, Emit: prop.EmitTrue, Callback: nil},
			"TasksCurrent":  {Value: uint32(0), Writable: false, Emit: prop.EmitTrue, Callback: nil},
			"ExecPath":      {Value: "", Writable: false, Emit: prop.EmitFalse, Callback: nil},
		},
	}
	p, err := prop.Export(conn, path, propsSpec)
	if err != nil {
		return nil, fmt.Errorf("exporting properties for %s: %w", name, err)
	}

	if err := conn.Export(introspect.NewIntrospectable(unitIntrospectNode(path)), path, "org.freedesktop.DBus.Introspectable"); err != nil {
		return nil, err
	}

	u := &Unit{Name: name, Path: path, conn: conn, props: p}

	if err := conn.Export(u, path, unitIface); err != nil {
		return nil, err
	}
	return u, nil
}

func (u *Unit) setActiveState(state, subState string) {
	u.props.SetMust(unitIface, "ActiveState", state)
	u.props.SetMust(unitIface, "SubState", subState)
}

// Nothing in a plain systemctl enable/disable/restart/mask/unmask/kill
// workflow calls these directly, but they're cheap to have in case some
// other D-Bus client does.

func (u *Unit) GetUnitFileState() (string, *dbus.Error) {
	v, derr := u.props.Get(unitIface, "UnitFileState")
	if derr != nil {
		return "", derr
	}
	if s, ok := v.Value().(string); ok {
		return s, nil
	}
	return "", nil
}

// Describe returns every property under org.freedesktop.systemd1.Unit as
// a single a{sv} blob - equivalent to calling Properties.GetAll yourself.
func (u *Unit) Describe() (map[string]dbus.Variant, *dbus.Error) {
	return u.props.GetAll(unitIface)
}

// Reload/Freeze/Thaw are momentary state pokes for whatever reads
// ActiveState/SubState afterward - there's nothing to actually reload,
// freeze, or thaw underneath a faked unit.
func (u *Unit) Reload(mode string) *dbus.Error {
	u.props.SetMust(unitIface, "SubState", "reloading")
	u.props.SetMust(unitIface, "SubState", "running")
	return nil
}

func (u *Unit) Freeze(mode string) *dbus.Error {
	u.props.SetMust(unitIface, "ActiveState", "frozen")
	return nil
}

func (u *Unit) Thaw() *dbus.Error {
	u.props.SetMust(unitIface, "ActiveState", "active")
	return nil
}

func unitIntrospectNode(path dbus.ObjectPath) *introspect.Node {
	arg := func(name, typ, dir string) introspect.Arg {
		return introspect.Arg{Name: name, Type: typ, Direction: dir}
	}
	return &introspect.Node{
		Name: string(path),
		Interfaces: []introspect.Interface{
			introspect.IntrospectData,
			prop.IntrospectData,
			{
				Name: unitIface,
				Methods: []introspect.Method{
					{Name: "GetUnitFileState", Args: []introspect.Arg{arg("state", "s", "out")}},
					{Name: "Describe", Args: []introspect.Arg{arg("props", "a{sv}", "out")}},
					{Name: "Reload", Args: []introspect.Arg{arg("mode", "s", "in")}},
					{Name: "Freeze", Args: []introspect.Arg{arg("mode", "s", "in")}},
					{Name: "Thaw"},
				},
				Properties: []introspect.Property{
					{Name: "Id", Type: "s", Access: "read"},
					{Name: "LoadState", Type: "s", Access: "read"},
					{Name: "ActiveState", Type: "s", Access: "read"},
					{Name: "SubState", Type: "s", Access: "read"},
					{Name: "Description", Type: "s", Access: "read"},
					{Name: "UnitFileState", Type: "s", Access: "read"},
					{Name: "Following", Type: "s", Access: "read"},
					{Name: "LoadError", Type: "s", Access: "read"},
				},
			},
			{
				Name: serviceIface,
				Properties: []introspect.Property{
					{Name: "StatusText", Type: "s", Access: "read"},
					{Name: "MainPID", Type: "u", Access: "read"},
					{Name: "MemoryCurrent", Type: "t", Access: "read"},
					{Name: "CPUUsageNSec", Type: "t", Access: "read"},
					{Name: "TasksCurrent", Type: "u", Access: "read"},
				},
			},
		},
	}
}

// `systemctl` (without --no-block) waits for a JobRemoved signal naming
// the job it triggered before it exits. We don't need real job
// concurrency - each job we "create" runs its action synchronously and
// immediately emits JobRemoved - but we still have to go through the
// motions (job path, JobNew, JobRemoved) or systemctl hangs forever
// waiting for a signal that never comes.

var jobIDCounter atomic.Uint32

func nextJobID() uint32 {
	return jobIDCounter.Add(1)
}

func jobPath(id uint32) dbus.ObjectPath {
	return dbus.ObjectPath(fmt.Sprintf("/org/freedesktop/systemd1/job/%d", id))
}

// runJob logs action's error but doesn't return it to the D-Bus caller -
// systemctl learns about failure via the job result string, not a method
// error.
func runJob(conn *dbus.Conn, unitName string, action func() error) dbus.ObjectPath {
	id := nextJobID()
	path := jobPath(id)

	_ = conn.Emit(managerPath, managerIface+".JobNew", id, path, unitName)

	result := "done"
	if err := action(); err != nil {
		slog.Error("job failed", "job_id", id, "unit", unitName, "error", err)
		result = "failed"
	}

	_ = conn.Emit(managerPath, managerIface+".JobRemoved", id, path, unitName, result)
	return path
}
