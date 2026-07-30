package main

import "github.com/godbus/dbus/v5"

// --- org.freedesktop.systemd1.Unit methods ---
//
// These mirror the extra methods in the fakesystemD reference project
// (GetUnitFileState, Describe, Reload, Freeze, Thaw). Nothing in
// unifi-core or a plain systemctl enable/disable/restart/mask/unmask/kill
// workflow calls these directly, but they're cheap to have in case
// something else on the bus (or a future unifi-core version) does.

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
//
// NOTE: this assumes prop.Properties.GetAll(iface) returns
// map[string]dbus.Variant directly (matching the a{sv} wire shape). If
// your godbus version's GetAll instead returns map[string]*prop.Prop (an
// internal bookkeeping type with a .Value field rather than the Variant
// itself), adjust this to wrap each entry: dbus.MakeVariant(p.Value).
func (u *Unit) Describe() (map[string]dbus.Variant, *dbus.Error) {
	return u.props.GetAll(unitIface), nil
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
