// Package hooks holds the Hook interface - the pluggable way
// systemd1-shim reacts to D-Bus unit commands - and every implementation
// of it (Noop, K8sRestarter, the Testing double for tests).
package hooks

import (
	"fmt"
	"sort"
	"strings"
)

// Hook reacts to D-Bus unit commands (StartUnit/StopUnit/RestartUnit/
// KillUnit). K8sRestarter is the only implementation that does anything
// real - it's just the first of what could be several ways to actually
// make "systemctl restart" restart something (Docker, a supervisor CLI,
// a webhook, ...). Add a new one by implementing this interface and
// registering a constructor in init() via register.
type Hook interface {
	Start(unitName string) error
	Stop(unitName string) error
	Restart(unitName string) error
	Kill(unitName string, signal int) error
}

// A hook loads whatever config it needs itself (env vars, files, ...) -
// there's no shared Config type, since what a hook needs is entirely up
// to that hook (see NewK8sRestarter for the "k8s" one).
type factory func() (Hook, error)

var registry = map[string]factory{}

// register makes a hook available for selection by name via the --hook
// CLI flag (see main.go's Load/Names calls). Call from an init() in the
// file defining the hook, the same way database/sql drivers register
// themselves.
func register(name string, f factory) {
	if _, exists := registry[name]; exists {
		panic("hook already registered: " + name)
	}
	registry[name] = f
}

// Names lists every registered hook, for --help text and Load's error
// message.
func Names() []string {
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Load constructs the named hook, or an error listing what's actually
// available if name isn't registered.
func Load(name string) (Hook, error) {
	f, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown hook %q (available: %s)", name, strings.Join(Names(), ", "))
	}
	return f()
}
