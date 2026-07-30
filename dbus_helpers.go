package main

import (
	"fmt"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/prop"
)

// propString/propUint32 read a property's current value back out of a
// prop.Properties instance. Used by the read-only listing methods
// (ListUnits, GetUnitByPID, ...) that need to report current state
// rather than just answer a Properties.Get for a single named property.
func propString(p *prop.Properties, iface, name string) (string, error) {
	v, derr := p.Get(iface, name)
	if derr != nil {
		return "", derr
	}
	s, _ := v.Value().(string)
	return s, nil
}

func propUint32(p *prop.Properties, iface, name string) (uint32, error) {
	v, derr := p.Get(iface, name)
	if derr != nil {
		return 0, derr
	}
	u, _ := v.Value().(uint32)
	return u, nil
}

func errNoSuchUnitForPID(pid uint32) error {
	return fmt.Errorf("no unit known for PID %d", pid)
}

func errNoSuchJob(id uint32) error {
	return fmt.Errorf("no such job: %d", id)
}
