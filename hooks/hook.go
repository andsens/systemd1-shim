// Package hooks holds the Hook interface - the pluggable way
// systemd1-shim reacts to D-Bus unit commands - and every implementation
// of it (Noop, K8sRestarter, the Testing double for tests).
package hooks

import (
	"fmt"
	"sort"
	"strings"
)

// Hook reacts to D-Bus unit commands. K8sRestarter is the only real
// implementation - add another by implementing this interface and
// calling register in an init().
type Hook interface {
	Start(unitName string) error
	Stop(unitName string) error
	Restart(unitName string) error
	Kill(unitName string, signal int) error
}

// No shared Config type - each hook loads whatever it needs itself
// (see NewK8sRestarter).
type factory func() (Hook, error)

var registry = map[string]factory{}

// register, called from each hook's init(), the same way database/sql
// drivers register themselves.
func register(name string, f factory) {
	if _, exists := registry[name]; exists {
		panic("hook already registered: " + name)
	}
	registry[name] = f
}

func Names() []string {
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func Load(name string) (Hook, error) {
	f, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown hook %q (available: %s)", name, strings.Join(Names(), ", "))
	}
	return f()
}
