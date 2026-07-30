package main

// Exercises Manager over a real D-Bus round trip, the same way godbus's
// own test suite does (see github.com/godbus/dbus/v5's export_test.go,
// conn_test.go): connect via dbus.ConnectSessionBus and call/observe
// signals across two real connections. No fakes/mocks of the bus itself.
//
// Requires an actual session bus reachable via $DBUS_SESSION_BUS_ADDRESS.
// Run under dbus-run-session, e.g.:
//
//	dbus-run-session -- go test ./...
//
// If no bus is reachable, tests skip rather than fail, since that's an
// environment precondition, not a code problem.

import (
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

const testBusName = "org.freedesktop.systemd1"

// connectTestBus opens a fresh connection to whatever session bus godbus
// would normally connect to - same call main.go makes for the system
// bus, just session instead so tests don't need root/an actual host bus.
func connectTestBus(t *testing.T) *dbus.Conn {
	t.Helper()
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		t.Skipf("no session bus reachable (%v) - run under `dbus-run-session -- go test ./...`", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// fakeHook records calls instead of touching Kubernetes, so tests assert
// against Manager/Unit's D-Bus-facing behavior in isolation.
type fakeHook struct {
	mu                          sync.Mutex
	started, stopped, restarted []string
	killed                      []killCall
	err                         error
}

type killCall struct {
	unit   string
	signal int
}

func (f *fakeHook) Start(unit string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, unit)
	return f.err
}

func (f *fakeHook) Stop(unit string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, unit)
	return f.err
}

func (f *fakeHook) Restart(unit string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restarted = append(f.restarted, unit)
	return f.err
}

func (f *fakeHook) Kill(unit string, signal int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killed = append(f.killed, killCall{unit, signal})
	return f.err
}

// newTestManager exports a Manager backed by hook on serverConn and
// claims testBusName, so a separate client connection can address it the
// same way `systemctl` addresses the real org.freedesktop.systemd1.
func newTestManager(t *testing.T, serverConn *dbus.Conn, hook Hook) *Manager {
	t.Helper()
	units := newUnitRegistry(serverConn)
	manager, err := newManager(serverConn, units, hook)
	if err != nil {
		t.Fatalf("newManager: %v", err)
	}
	if err := serverConn.Export(manager, managerPath, managerIface); err != nil {
		t.Fatalf("exporting Manager: %v", err)
	}
	reply, err := serverConn.RequestName(testBusName, dbus.NameFlagDoNotQueue)
	if err != nil {
		t.Fatalf("RequestName: %v", err)
	}
	if reply != dbus.RequestNameReplyPrimaryOwner {
		t.Fatalf("RequestName: reply=%d, expected PrimaryOwner", reply)
	}
	return manager
}

func TestStartUnit_OverDBus(t *testing.T) {
	serverConn := connectTestBus(t)
	hook := &fakeHook{}
	newTestManager(t, serverConn, hook)

	clientConn := connectTestBus(t)

	sigc := make(chan *dbus.Signal, 10)
	clientConn.Signal(sigc)
	if err := clientConn.AddMatchSignal(dbus.WithMatchInterface(managerIface)); err != nil {
		t.Fatalf("AddMatchSignal: %v", err)
	}

	obj := clientConn.Object(testBusName, managerPath)
	var jobPath dbus.ObjectPath
	if err := obj.Call(managerIface+".StartUnit", 0, "my-app.service", "replace").Store(&jobPath); err != nil {
		t.Fatalf("StartUnit call: %v", err)
	}
	if jobPath == "" {
		t.Fatal("StartUnit returned an empty job path")
	}

	hook.mu.Lock()
	started := append([]string(nil), hook.started...)
	hook.mu.Unlock()
	if len(started) != 1 || started[0] != "my-app.service" {
		t.Fatalf("hook.started = %v, want [my-app.service]", started)
	}

	// Real systemctl blocks on JobRemoved before returning - assert both
	// signals actually cross the bus, in order, not just that the method
	// call itself returned. Connection-directed signals unrelated to
	// managerIface (e.g. NameAcquired, from RequestName) arrive on the
	// same channel regardless of AddMatchSignal, so skip past those.
	wantSignals := []string{"JobNew", "JobRemoved"}
	for _, want := range wantSignals {
		for {
			select {
			case sig := <-sigc:
				if sig.Name == managerIface+"."+want {
					goto next
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("timed out waiting for signal %q", want)
			}
		}
	next:
	}

	var activeState string
	unitObj := clientConn.Object(testBusName, unitPath("my-app.service"))
	if err := unitObj.Call("org.freedesktop.DBus.Properties.Get", 0, unitIface, "ActiveState").Store(&activeState); err != nil {
		t.Fatalf("reading ActiveState: %v", err)
	}
	if activeState != "active" {
		t.Fatalf("ActiveState = %q, want %q", activeState, "active")
	}
}

func TestKillUnit_OverDBus(t *testing.T) {
	serverConn := connectTestBus(t)
	hook := &fakeHook{}
	newTestManager(t, serverConn, hook)

	clientConn := connectTestBus(t)
	obj := clientConn.Object(testBusName, managerPath)

	// KillUnit is synchronous in real systemd too (no Job) - the call
	// itself must not return until the hook has run.
	call := obj.Call(managerIface+".KillUnit", 0, "my-app.service", "all", int32(9))
	if call.Err != nil {
		t.Fatalf("KillUnit call: %v", call.Err)
	}

	hook.mu.Lock()
	killed := append([]killCall(nil), hook.killed...)
	hook.mu.Unlock()
	if len(killed) != 1 || killed[0] != (killCall{"my-app.service", 9}) {
		t.Fatalf("hook.killed = %v, want [{my-app.service 9}]", killed)
	}
}
