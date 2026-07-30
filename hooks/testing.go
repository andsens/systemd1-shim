package hooks

import "sync"

// Testing is a Hook that records every call instead of doing anything,
// for tests asserting on Manager/Unit's D-Bus-facing behavior without a
// real backend. Not registered for --hook selection - construct it
// directly: hook := &hooks.Testing{}.
type Testing struct {
	mu                          sync.Mutex
	started, stopped, restarted []string
	killed                      []TestingKillCall

	// Err, if set, is returned by every method call instead of nil.
	Err error
}

// TestingKillCall records one Kill call's arguments.
type TestingKillCall struct {
	Unit   string
	Signal int
}

func (t *Testing) Start(unit string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.started = append(t.started, unit)
	return t.Err
}

func (t *Testing) Stop(unit string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stopped = append(t.stopped, unit)
	return t.Err
}

func (t *Testing) Restart(unit string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.restarted = append(t.restarted, unit)
	return t.Err
}

func (t *Testing) Kill(unit string, signal int) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.killed = append(t.killed, TestingKillCall{unit, signal})
	return t.Err
}

// Started, Stopped, Restarted, and Killed return snapshots of recorded
// calls - safe to read concurrently with the Hook methods above.

func (t *Testing) Started() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.started...)
}

func (t *Testing) Stopped() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.stopped...)
}

func (t *Testing) Restarted() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.restarted...)
}

func (t *Testing) Killed() []TestingKillCall {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]TestingKillCall(nil), t.killed...)
}
