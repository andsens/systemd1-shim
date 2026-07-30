package main

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
	"github.com/godbus/dbus/v5/prop"
)

const (
	unitIface    = "org.freedesktop.systemd1.Unit"
	serviceIface = "org.freedesktop.systemd1.Service"
	managerIface = "org.freedesktop.systemd1.Manager"
)

// unitPath turns "foo.service" (or "foo") into a stable, valid D-Bus object
// path segment. Real systemd uses its own escaping scheme; we just need
// something bijective and legal, not byte-compatible with real systemd.
func unitPath(name string) dbus.ObjectPath {
	base := strings.TrimSuffix(name, ".service")
	var b strings.Builder
	for _, r := range base {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			fmt.Fprintf(&b, "_%02x", r)
		}
	}
	return dbus.ObjectPath("/org/freedesktop/systemd1/unit/" + b.String())
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

func (u *Unit) setStatusText(text string) {
	u.props.SetMust(serviceIface, "StatusText", text)
}

func (u *Unit) setMainPID(pid uint32) {
	u.props.SetMust(serviceIface, "MainPID", pid)
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

// UnitRegistry is the shared, lazily-populated map of unit name -> Unit.
type UnitRegistry struct {
	mu    sync.Mutex
	conn  *dbus.Conn
	units map[string]*Unit // keyed by normalized "name.service"
}

func newUnitRegistry(conn *dbus.Conn) *UnitRegistry {
	return &UnitRegistry{conn: conn, units: make(map[string]*Unit)}
}

func normalizeUnitName(name string) string {
	if !strings.HasSuffix(name, ".service") {
		return name + ".service"
	}
	return name
}

// getOrCreate doesn't distinguish loaded/not-loaded like real systemd
// does - everything springs into existence on first reference, defaulting
// to a healthy state (see README for why).
func (r *UnitRegistry) getOrCreate(name string) (*Unit, error) {
	key := normalizeUnitName(name)
	r.mu.Lock()
	defer r.mu.Unlock()
	if u, ok := r.units[key]; ok {
		return u, nil
	}
	u, err := newUnit(r.conn, key)
	if err != nil {
		return nil, err
	}
	r.units[key] = u
	return u, nil
}

func (r *UnitRegistry) get(name string) (*Unit, bool) {
	key := normalizeUnitName(name)
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.units[key]
	return u, ok
}

// all snapshots under the lock, so it's safe to call while other
// goroutines are creating units.
func (r *UnitRegistry) all() []*Unit {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Unit, 0, len(r.units))
	for _, u := range r.units {
		out = append(out, u)
	}
	return out
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
