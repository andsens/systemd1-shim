package hooks

import "log/slog"

// Noop is a Hook that does nothing beyond logging - no Kubernetes, no
// external dependency of any kind. Useful for running systemd1-shim
// standalone to poke at the D-Bus/systemctl-facing behavior in
// isolation, without needing a cluster to even start the process.
type Noop struct{}

func init() {
	register("noop", func() (Hook, error) { return Noop{}, nil })
}

func (Noop) Start(unitName string) error {
	slog.Info("noop hook: start", "unit", unitName)
	return nil
}

func (Noop) Stop(unitName string) error {
	slog.Info("noop hook: stop", "unit", unitName)
	return nil
}

func (Noop) Restart(unitName string) error {
	slog.Info("noop hook: restart", "unit", unitName)
	return nil
}

func (Noop) Kill(unitName string, signal int) error {
	slog.Info("noop hook: kill", "unit", unitName, "signal", signal)
	return nil
}
