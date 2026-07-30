package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
	"github.com/godbus/dbus/v5/prop"

	"github.com/andsens/systemd1-shim/hooks"
)

const managerPath = dbus.ObjectPath("/org/freedesktop/systemd1")

// --- Wire-format struct types for the array-of-struct return values below ---
// Field order matters: godbus marshals exported struct fields in
// declaration order, so each of these must match its D-Bus signature
// exactly (see the matching entries in managerIntrospectNode).

// UnitFileChange mirrors systemd's `(sss)` change-tuple: (type, file,
// destination). systemctl doesn't do anything interesting with the
// contents for our purposes - it's fine for this to always come back
// empty; only the *shape* needs to be right so the reply decodes.
type UnitFileChange struct {
	Type, File, Destination string
}

// UnitListEntry: a(ssssssouso) - ListUnits / ListUnitsFiltered.
type UnitListEntry struct {
	Name, Description, LoadState, ActiveState, SubState, Following string
	UnitPath                                                       dbus.ObjectPath
	JobID                                                          uint32
	JobType                                                        string
	JobPath                                                        dbus.ObjectPath
}

// UnitFileEntry: a(ss) - ListUnitFiles.
type UnitFileEntry struct {
	Path, State string
}

// JobListEntry: a(usssoo) - ListJobs.
type JobListEntry struct {
	ID                   uint32
	Unit, JobType, State string
	JobPath, UnitPath    dbus.ObjectPath
}

// PropertyAssignment: a(sv) - SetUnitProperties input.
type PropertyAssignment struct {
	Name  string
	Value dbus.Variant
}

// ProcessEntry: a(sus) - GetUnitProcesses.
type ProcessEntry struct {
	ControlGroup string
	PID          uint32
	CommandLine  string
}

// propString reads a property's current value back out of a
// prop.Properties instance, for the listing methods below that need to
// report current state (ListUnits, ...).
func propString(p *prop.Properties, iface, name string) (string, error) {
	v, derr := p.Get(iface, name)
	if derr != nil {
		return "", derr
	}
	s, _ := v.Value().(string)
	return s, nil
}

func errNoSuchUnitForPID(pid uint32) error {
	return fmt.Errorf("no unit known for PID %d", pid)
}

func errNoSuchJob(id uint32) error {
	return fmt.Errorf("no such job: %d", id)
}

// Manager implements org.freedesktop.systemd1.Manager. Method names below
// must exactly match the real D-Bus method names - godbus dispatches
// exported calls by matching this Go method name against the D-Bus method
// name being invoked.
type Manager struct {
	conn  *dbus.Conn
	hook  hooks.Hook
	props *prop.Properties

	mu    sync.Mutex
	units map[string]*Unit // keyed by normalized "name.service"
}

func newManager(conn *dbus.Conn, hook hooks.Hook) (*Manager, error) {
	m := &Manager{conn: conn, hook: hook, units: make(map[string]*Unit)}

	propsSpec := prop.Map{
		managerIface: {
			// "running" so `systemctl is-system-running --wait` returns
			// immediately instead of blocking on a state transition that
			// will never happen here.
			"SystemState":    {Value: "running", Writable: false, Emit: prop.EmitTrue, Callback: nil},
			"Version":        {Value: "247", Writable: false, Emit: prop.EmitFalse, Callback: nil},
			"Features":       {Value: "systemd1-shim", Writable: false, Emit: prop.EmitFalse, Callback: nil},
			"Virtualization": {Value: "container", Writable: false, Emit: prop.EmitFalse, Callback: nil},
			"Architecture":   {Value: goArchToSystemdArch(), Writable: false, Emit: prop.EmitFalse, Callback: nil},
		},
	}
	p, err := prop.Export(conn, managerPath, propsSpec)
	if err != nil {
		return nil, err
	}
	m.props = p

	if err := conn.Export(introspect.NewIntrospectable(managerIntrospectNode()), managerPath, "org.freedesktop.DBus.Introspectable"); err != nil {
		return nil, err
	}
	return m, nil
}

func normalizeUnitName(name string) string {
	if !strings.HasSuffix(name, ".service") {
		return name + ".service"
	}
	return name
}

// getOrCreateUnit doesn't distinguish loaded/not-loaded like real systemd
// does - everything springs into existence on first reference, defaulting
// to a healthy state (see README for why).
func (m *Manager) getOrCreateUnit(name string) (*Unit, error) {
	key := normalizeUnitName(name)
	m.mu.Lock()
	defer m.mu.Unlock()
	if u, ok := m.units[key]; ok {
		return u, nil
	}
	u, err := newUnit(m.conn, key)
	if err != nil {
		return nil, err
	}
	m.units[key] = u
	return u, nil
}

func (m *Manager) getUnit(name string) (*Unit, bool) {
	key := normalizeUnitName(name)
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.units[key]
	return u, ok
}

// allUnits snapshots under the lock, so it's safe to call while other
// goroutines are creating units.
func (m *Manager) allUnits() []*Unit {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Unit, 0, len(m.units))
	for _, u := range m.units {
		out = append(out, u)
	}
	return out
}

// --- Methods actually reachable from real systemctl invocations and other D-Bus clients ---

func (m *Manager) Subscribe() *dbus.Error {
	// Real systemd only starts emitting Job*/PropertiesChanged after this;
	// we emit unconditionally regardless, so this is a deliberate no-op.
	return nil
}

func (m *Manager) Unsubscribe() *dbus.Error {
	return nil
}

func (m *Manager) LoadUnit(name string) (dbus.ObjectPath, *dbus.Error) {
	u, err := m.getOrCreateUnit(name)
	if err != nil {
		return "", dbus.MakeFailedError(err)
	}
	return u.Path, nil
}

// GetUnit should, per real semantics, fail if the unit isn't already
// loaded. We don't track that distinction, so this behaves identically
// to LoadUnit - fine, since nothing relies on GetUnit failing here.
func (m *Manager) GetUnit(name string) (dbus.ObjectPath, *dbus.Error) {
	return m.LoadUnit(name)
}

func (m *Manager) StartUnit(name, mode string) (dbus.ObjectPath, *dbus.Error) {
	u, err := m.getOrCreateUnit(name)
	if err != nil {
		return "", dbus.MakeFailedError(err)
	}
	if u.masked {
		return "", dbus.NewError("org.freedesktop.systemd1.UnitMasked", []interface{}{u.Name + " is masked"})
	}
	path := runJob(m.conn, u.Name, func() error {
		u.setActiveState("activating", "start")
		defer u.setActiveState("active", "running")
		return m.hook.Start(u.Name)
	})
	return path, nil
}

func (m *Manager) StopUnit(name, mode string) (dbus.ObjectPath, *dbus.Error) {
	u, err := m.getOrCreateUnit(name)
	if err != nil {
		return "", dbus.MakeFailedError(err)
	}
	path := runJob(m.conn, u.Name, func() error {
		u.setActiveState("deactivating", "stop")
		defer u.setActiveState("inactive", "dead")
		return m.hook.Stop(u.Name)
	})
	return path, nil
}

func (m *Manager) RestartUnit(name, mode string) (dbus.ObjectPath, *dbus.Error) {
	u, err := m.getOrCreateUnit(name)
	if err != nil {
		return "", dbus.MakeFailedError(err)
	}
	if u.masked {
		return "", dbus.NewError("org.freedesktop.systemd1.UnitMasked", []interface{}{u.Name + " is masked"})
	}
	path := runJob(m.conn, u.Name, func() error {
		u.setActiveState("activating", "restart")
		defer u.setActiveState("active", "running")
		return m.hook.Restart(u.Name)
	})
	return path, nil
}

// KillUnit is synchronous in real systemd too (no Job) - matches
// `systemctl kill <unit>.service`, default whom="all" signal=SIGTERM(15).
func (m *Manager) KillUnit(name, whom string, signal int32) *dbus.Error {
	u, err := m.getOrCreateUnit(name)
	if err != nil {
		return dbus.MakeFailedError(err)
	}
	if err := m.hook.Kill(u.Name, int(signal)); err != nil {
		return dbus.MakeFailedError(err)
	}
	u.setActiveState("inactive", "dead")
	return nil
}

func (m *Manager) EnableUnitFiles(files []string, runtime, force bool) (bool, []UnitFileChange, *dbus.Error) {
	for _, f := range files {
		if u, err := m.getOrCreateUnit(f); err == nil {
			u.masked = false
		}
	}
	return false, []UnitFileChange{}, nil
}

func (m *Manager) DisableUnitFiles(files []string, runtime bool) ([]UnitFileChange, *dbus.Error) {
	return []UnitFileChange{}, nil
}

func (m *Manager) MaskUnitFiles(files []string, runtime, force bool) ([]UnitFileChange, *dbus.Error) {
	for _, f := range files {
		u, err := m.getOrCreateUnit(f)
		if err != nil {
			return nil, dbus.MakeFailedError(err)
		}
		u.masked = true
		u.setActiveState("inactive", "dead")
	}
	return []UnitFileChange{}, nil
}

func (m *Manager) UnmaskUnitFiles(files []string, runtime bool) ([]UnitFileChange, *dbus.Error) {
	for _, f := range files {
		if u, ok := m.getUnit(f); ok {
			u.masked = false
		}
	}
	return []UnitFileChange{}, nil
}

func (m *Manager) Reload() *dbus.Error {
	return nil
}

// --- Informational methods ---
// Real systemd only exposes these as Manager *properties*
// (Version/Features/Virtualization/Architecture/Environment - see the
// propsSpec in newManager). fakesystemD additionally exposes them as
// plain callable methods; these are here purely for parity with that,
// in case something calls them as methods instead of reading the
// property.

func (m *Manager) GetVersion() (string, *dbus.Error) {
	return "247", nil
}

func (m *Manager) GetFeatures() (string, *dbus.Error) {
	return "systemd1-shim", nil
}

func (m *Manager) GetVirtualization() (string, *dbus.Error) {
	return "container", nil
}

func (m *Manager) GetArchitecture() (string, *dbus.Error) {
	return goArchToSystemdArch(), nil
}

func (m *Manager) GetEnvironment() ([]string, *dbus.Error) {
	return os.Environ(), nil
}

func goArchToSystemdArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x86-64"
	case "386":
		return "x86"
	case "arm64":
		return "arm64"
	case "arm":
		return "arm"
	default:
		return runtime.GOARCH
	}
}

// --- Listing / lifecycle-adjacent methods ---

func (m *Manager) Reexecute() *dbus.Error {
	// Nothing to re-exec into - this process already is the whole
	// "systemd" as far as anything on the bus can tell.
	return nil
}

func (m *Manager) ResetFailedUnit(name string) *dbus.Error {
	if u, ok := m.getUnit(name); ok {
		u.setActiveState("active", "running")
	}
	return nil
}

func (m *Manager) GetUnitFileState(name string) (string, *dbus.Error) {
	if u, ok := m.getUnit(name); ok {
		return u.GetUnitFileState()
	}
	return "not-found", nil
}

func (m *Manager) ListUnitFiles() ([]UnitFileEntry, *dbus.Error) {
	units := m.allUnits()
	out := make([]UnitFileEntry, 0, len(units))
	for _, u := range units {
		state := "enabled"
		if u.masked {
			state = "masked"
		}
		out = append(out, UnitFileEntry{Path: u.Name, State: state})
	}
	return out, nil
}

func (m *Manager) ListUnits() ([]UnitListEntry, *dbus.Error) {
	units := m.allUnits()
	out := make([]UnitListEntry, 0, len(units))
	for _, u := range units {
		activeState, _ := propString(u.props, unitIface, "ActiveState")
		subState, _ := propString(u.props, unitIface, "SubState")
		loadState, _ := propString(u.props, unitIface, "LoadState")
		description, _ := propString(u.props, unitIface, "Description")
		out = append(out, UnitListEntry{
			Name:        u.Name,
			Description: description,
			LoadState:   loadState,
			ActiveState: activeState,
			SubState:    subState,
			Following:   "",
			UnitPath:    u.Path,
			JobID:       0,
			JobType:     "",
			JobPath:     dbus.ObjectPath("/"),
		})
	}
	return out, nil
}

func (m *Manager) ListUnitsFiltered(states []string) ([]UnitListEntry, *dbus.Error) {
	// Real systemd filters by ActiveState here; we don't bother since
	// nothing in the traced call pattern uses this - return everything.
	return m.ListUnits()
}

// GetUnitByPID / GetUnitProcesses: this shim never learns a unit's
// MainPID - nothing here execs a process locally to observe one, and
// hooks run out-of-process (see README.md). No PID is ever tracked, so
// no PID can ever match and no process list is ever non-empty. Always
// "not found" / empty is the honest answer, not a stub.
func (m *Manager) GetUnitByPID(pid uint32) (dbus.ObjectPath, *dbus.Error) {
	return "", dbus.MakeFailedError(errNoSuchUnitForPID(pid))
}

// ListJobs / GetJob: every job we create in runJob() runs to completion
// and emits JobRemoved *before* the method call that created it even
// returns (see unit.go), so by the time anything could call ListJobs
// there's structurally nothing pending. Returning empty / "not found"
// is the realistic answer here, not a stub.
func (m *Manager) ListJobs() ([]JobListEntry, *dbus.Error) {
	return []JobListEntry{}, nil
}

func (m *Manager) GetJob(id uint32) (dbus.ObjectPath, *dbus.Error) {
	return "", dbus.MakeFailedError(errNoSuchJob(id))
}

func (m *Manager) SetUnitProperties(name string, runtimeOnly bool, properties []PropertyAssignment) *dbus.Error {
	// Accepted and ignored - nothing currently reads arbitrary
	// runtime-set properties back.
	return nil
}

func (m *Manager) GetUnitProcesses(name string) ([]ProcessEntry, *dbus.Error) {
	return []ProcessEntry{}, nil
}

func managerIntrospectNode() *introspect.Node {
	arg := func(name, typ, dir string) introspect.Arg {
		return introspect.Arg{Name: name, Type: typ, Direction: dir}
	}
	return &introspect.Node{
		Name: string(managerPath),
		Interfaces: []introspect.Interface{
			introspect.IntrospectData,
			prop.IntrospectData,
			{
				Name: managerIface,
				Methods: []introspect.Method{
					{Name: "Subscribe"},
					{Name: "Unsubscribe"},
					{Name: "LoadUnit", Args: []introspect.Arg{
						arg("name", "s", "in"), arg("unit", "o", "out"),
					}},
					{Name: "GetUnit", Args: []introspect.Arg{
						arg("name", "s", "in"), arg("unit", "o", "out"),
					}},
					{Name: "StartUnit", Args: []introspect.Arg{
						arg("name", "s", "in"), arg("mode", "s", "in"), arg("job", "o", "out"),
					}},
					{Name: "StopUnit", Args: []introspect.Arg{
						arg("name", "s", "in"), arg("mode", "s", "in"), arg("job", "o", "out"),
					}},
					{Name: "RestartUnit", Args: []introspect.Arg{
						arg("name", "s", "in"), arg("mode", "s", "in"), arg("job", "o", "out"),
					}},
					{Name: "KillUnit", Args: []introspect.Arg{
						arg("name", "s", "in"), arg("whom", "s", "in"), arg("signal", "i", "in"),
					}},
					{Name: "EnableUnitFiles", Args: []introspect.Arg{
						arg("files", "as", "in"), arg("runtime", "b", "in"), arg("force", "b", "in"),
						arg("carries_install_info", "b", "out"), arg("changes", "a(sss)", "out"),
					}},
					{Name: "DisableUnitFiles", Args: []introspect.Arg{
						arg("files", "as", "in"), arg("runtime", "b", "in"),
						arg("changes", "a(sss)", "out"),
					}},
					{Name: "MaskUnitFiles", Args: []introspect.Arg{
						arg("files", "as", "in"), arg("runtime", "b", "in"), arg("force", "b", "in"),
						arg("changes", "a(sss)", "out"),
					}},
					{Name: "UnmaskUnitFiles", Args: []introspect.Arg{
						arg("files", "as", "in"), arg("runtime", "b", "in"),
						arg("changes", "a(sss)", "out"),
					}},
					{Name: "Reload"},
					{Name: "GetVersion", Args: []introspect.Arg{arg("version", "s", "out")}},
					{Name: "GetFeatures", Args: []introspect.Arg{arg("features", "s", "out")}},
					{Name: "GetVirtualization", Args: []introspect.Arg{arg("virt", "s", "out")}},
					{Name: "GetArchitecture", Args: []introspect.Arg{arg("arch", "s", "out")}},
					{Name: "GetEnvironment", Args: []introspect.Arg{arg("env", "as", "out")}},
					{Name: "Reexecute"},
					{Name: "ResetFailedUnit", Args: []introspect.Arg{arg("name", "s", "in")}},
					{Name: "GetUnitFileState", Args: []introspect.Arg{
						arg("name", "s", "in"), arg("state", "s", "out"),
					}},
					{Name: "ListUnitFiles", Args: []introspect.Arg{
						arg("files", "a(ss)", "out"),
					}},
					{Name: "ListUnits", Args: []introspect.Arg{
						arg("units", "a(ssssssouso)", "out"),
					}},
					{Name: "ListUnitsFiltered", Args: []introspect.Arg{
						arg("states", "as", "in"), arg("units", "a(ssssssouso)", "out"),
					}},
					{Name: "GetUnitByPID", Args: []introspect.Arg{
						arg("pid", "u", "in"), arg("unit", "o", "out"),
					}},
					{Name: "ListJobs", Args: []introspect.Arg{
						arg("jobs", "a(usssoo)", "out"),
					}},
					{Name: "GetJob", Args: []introspect.Arg{
						arg("id", "u", "in"), arg("job", "o", "out"),
					}},
					{Name: "SetUnitProperties", Args: []introspect.Arg{
						arg("name", "s", "in"), arg("runtime", "b", "in"), arg("properties", "a(sv)", "in"),
					}},
					{Name: "GetUnitProcesses", Args: []introspect.Arg{
						arg("name", "s", "in"), arg("processes", "a(sus)", "out"),
					}},
				},
				Signals: []introspect.Signal{
					{Name: "UnitFilesChanged"},
					{Name: "JobNew", Args: []introspect.Arg{
						arg("id", "u", "out"), arg("job", "o", "out"), arg("unit", "s", "out"),
					}},
					{Name: "JobRemoved", Args: []introspect.Arg{
						arg("id", "u", "out"), arg("job", "o", "out"), arg("unit", "s", "out"), arg("result", "s", "out"),
					}},
				},
				Properties: []introspect.Property{
					{Name: "SystemState", Type: "s", Access: "read"},
					{Name: "Version", Type: "s", Access: "read"},
					{Name: "Features", Type: "s", Access: "read"},
					{Name: "Virtualization", Type: "s", Access: "read"},
					{Name: "Architecture", Type: "s", Access: "read"},
				},
			},
		},
	}
}
