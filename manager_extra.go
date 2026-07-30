package main

import (
	"os"
	"runtime"

	"github.com/godbus/dbus/v5"
)

// --- Wire-format struct types for the array-of-struct return values below ---
// Field order matters: godbus marshals exported struct fields in
// declaration order, so each of these must match its D-Bus signature
// exactly (see the matching entries in managerIntrospectNode).

// UnitListEntry: a(ssssssouso) - ListUnits / ListUnitsFiltered.
type UnitListEntry struct {
	Name        string
	Description string
	LoadState   string
	ActiveState string
	SubState    string
	Following   string
	UnitPath    dbus.ObjectPath
	JobID       uint32
	JobType     string
	JobPath     dbus.ObjectPath
}

// UnitFileEntry: a(ss) - ListUnitFiles.
type UnitFileEntry struct {
	Path  string
	State string
}

// JobListEntry: a(usssoo) - ListJobs.
type JobListEntry struct {
	ID       uint32
	Unit     string
	JobType  string
	State    string
	JobPath  dbus.ObjectPath
	UnitPath dbus.ObjectPath
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
	if u, ok := m.units.get(name); ok {
		u.setActiveState("active", "running")
	}
	return nil
}

func (m *Manager) GetUnitFileState(name string) (string, *dbus.Error) {
	if u, ok := m.units.get(name); ok {
		return u.GetUnitFileState()
	}
	return "not-found", nil
}

func (m *Manager) ListUnitFiles() ([]UnitFileEntry, *dbus.Error) {
	units := m.units.all()
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
	units := m.units.all()
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

func (m *Manager) GetUnitByPID(pid uint32) (dbus.ObjectPath, *dbus.Error) {
	for _, u := range m.units.all() {
		mainPID, _ := propUint32(u.props, serviceIface, "MainPID")
		if mainPID == pid {
			return u.Path, nil
		}
	}
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
	u, ok := m.units.get(name)
	if !ok {
		return []ProcessEntry{}, nil
	}
	pid, _ := propUint32(u.props, serviceIface, "MainPID")
	if pid == 0 {
		return []ProcessEntry{}, nil
	}
	return []ProcessEntry{{ControlGroup: "/" + u.Name, PID: pid, CommandLine: ""}}, nil
}
