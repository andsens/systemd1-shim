package main

import (
	"fmt"
	"sort"
	"strings"
)

// Hook is the pluggable interface for reacting to D-Bus unit commands
// (StartUnit/StopUnit/RestartUnit/KillUnit). The Kubernetes hook
// (k8s_restart.go) is the only implementation today - it's just the first
// of what could be several ways to actually make "systemctl restart"
// restart something (Docker, a supervisor CLI, a webhook, ...). Add a new
// one by implementing this interface and registering a constructor in
// init() via registerHook.
type Hook interface {
	Start(unitName string) error
	Stop(unitName string) error
	Restart(unitName string) error
	Kill(unitName string, signal int) error
}

// A hook loads whatever config it needs itself (env vars, files, ...) -
// there's no shared Config type, since what a hook needs is entirely up
// to that hook (see NewK8sRestarter in k8s_restart.go for the "k8s" one).
type hookFactory func() (Hook, error)

var hookRegistry = map[string]hookFactory{}

// registerHook makes a hook available for selection by name via the
// --hook CLI flag (see main.go). Call from an init() in the file defining
// the hook, the same way database/sql drivers register themselves.
func registerHook(name string, factory hookFactory) {
	if _, exists := hookRegistry[name]; exists {
		panic("hook already registered: " + name)
	}
	hookRegistry[name] = factory
}

func hookNames() []string {
	names := make([]string, 0, len(hookRegistry))
	for name := range hookRegistry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func loadHook(name string) (Hook, error) {
	factory, ok := hookRegistry[name]
	if !ok {
		return nil, fmt.Errorf("unknown hook %q (available: %s)", name, strings.Join(hookNames(), ", "))
	}
	return factory()
}
