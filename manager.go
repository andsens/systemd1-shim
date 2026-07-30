package main

import (
	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
	"github.com/godbus/dbus/v5/prop"
)

const managerPath = dbus.ObjectPath("/org/freedesktop/systemd1")

// UnitFileChange mirrors systemd's `(sss)` change-tuple: (type, file,
// destination). systemctl doesn't do anything interesting with the
// contents for our purposes - it's fine for this to always come back
// empty; only the *shape* needs to be right so the reply decodes.
type UnitFileChange struct {
	Type        string
	File        string
	Destination string
}

// Manager implements org.freedesktop.systemd1.Manager. Method names below
// must exactly match the real D-Bus method names - godbus dispatches
// exported calls by matching this Go method name against the D-Bus method
// name being invoked.
type Manager struct {
	conn      *dbus.Conn
	units     *UnitRegistry
	restarter Restarter
	props     *prop.Properties
}

func newManager(conn *dbus.Conn, units *UnitRegistry, restarter Restarter) (*Manager, error) {
	m := &Manager{conn: conn, units: units, restarter: restarter}

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

// --- Methods actually reachable from real systemctl invocations + unifi-core's own D-Bus client ---

func (m *Manager) Subscribe() *dbus.Error {
	// Real systemd only starts emitting Job*/PropertiesChanged after this;
	// we emit unconditionally regardless, so this is a deliberate no-op.
	return nil
}

func (m *Manager) Unsubscribe() *dbus.Error {
	return nil
}

func (m *Manager) LoadUnit(name string) (dbus.ObjectPath, *dbus.Error) {
	u, err := m.units.getOrCreate(name)
	if err != nil {
		return "", dbus.MakeFailedError(err)
	}
	return u.Path, nil
}

// GetUnit should, per real semantics, fail if the unit isn't already
// loaded. We don't track that distinction - everything is created on
// first reference regardless of which method asked for it - so this
// behaves identically to LoadUnit. Fine for our purposes: nothing here
// relies on GetUnit failing for an unknown unit.
func (m *Manager) GetUnit(name string) (dbus.ObjectPath, *dbus.Error) {
	return m.LoadUnit(name)
}

func (m *Manager) StartUnit(name, mode string) (dbus.ObjectPath, *dbus.Error) {
	u, err := m.units.getOrCreate(name)
	if err != nil {
		return "", dbus.MakeFailedError(err)
	}
	if u.masked {
		return "", dbus.NewError("org.freedesktop.systemd1.UnitMasked", []interface{}{u.Name + " is masked"})
	}
	path := runJob(m.conn, u.Name, func() error {
		u.setActiveState("activating", "start")
		defer u.setActiveState("active", "running")
		return m.restarter.Start(u.Name)
	})
	return path, nil
}

func (m *Manager) StopUnit(name, mode string) (dbus.ObjectPath, *dbus.Error) {
	u, err := m.units.getOrCreate(name)
	if err != nil {
		return "", dbus.MakeFailedError(err)
	}
	path := runJob(m.conn, u.Name, func() error {
		u.setActiveState("deactivating", "stop")
		defer u.setActiveState("inactive", "dead")
		return m.restarter.Stop(u.Name)
	})
	return path, nil
}

func (m *Manager) RestartUnit(name, mode string) (dbus.ObjectPath, *dbus.Error) {
	u, err := m.units.getOrCreate(name)
	if err != nil {
		return "", dbus.MakeFailedError(err)
	}
	if u.masked {
		return "", dbus.NewError("org.freedesktop.systemd1.UnitMasked", []interface{}{u.Name + " is masked"})
	}
	path := runJob(m.conn, u.Name, func() error {
		u.setActiveState("activating", "restart")
		defer u.setActiveState("active", "running")
		return m.restarter.Restart(u.Name)
	})
	return path, nil
}

// KillUnit is synchronous in real systemd too (no Job) - matches
// `systemctl kill <unit>.service`, default whom="all" signal=SIGTERM(15).
func (m *Manager) KillUnit(name, whom string, signal int32) *dbus.Error {
	u, err := m.units.getOrCreate(name)
	if err != nil {
		return dbus.MakeFailedError(err)
	}
	if err := m.restarter.Kill(u.Name, int(signal)); err != nil {
		return dbus.MakeFailedError(err)
	}
	u.setActiveState("inactive", "dead")
	return nil
}

func (m *Manager) EnableUnitFiles(files []string, runtime, force bool) (bool, []UnitFileChange, *dbus.Error) {
	for _, f := range files {
		if u, err := m.units.getOrCreate(f); err == nil {
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
		u, err := m.units.getOrCreate(f)
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
		if u, ok := m.units.get(f); ok {
			u.masked = false
		}
	}
	return []UnitFileChange{}, nil
}

func (m *Manager) Reload() *dbus.Error {
	return nil
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
