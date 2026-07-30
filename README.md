# systemd1-shim

**Disclaimer: This is completely vibe-coded with Claude AI, so do not expect exceptional code-quality**

A fake `org.freedesktop.systemd1` D-Bus service: `systemctl` and any D-Bus
client using the standard systemd1 API can operate against a shared D-Bus
socket with no real systemd anywhere in the pod/host. Start/stop/restart/
kill land on a pluggable **hook** (see "Hooks" below) that does the actual
work of reacting to those commands.

## What's covered

**D-Bus, `org.freedesktop.systemd1.Manager`:**

- Core surface: `Subscribe`, `Unsubscribe`, `LoadUnit`, `GetUnit`,
  `StartUnit`, `StopUnit`, `RestartUnit`, `KillUnit`, `EnableUnitFiles`,
  `DisableUnitFiles`, `MaskUnitFiles`, `UnmaskUnitFiles`, `Reload`;
  property `SystemState` (→ `systemctl is-system-running`); signals
  `JobNew`/`JobRemoved` (so `systemctl` unblocks after `enable --now`/
  `disable --now`/`restart`/`mask --now` instead of hanging).
- Broader surface for parity with the `fakesystemD` reference project:
  `GetVersion`, `GetFeatures`, `GetVirtualization`, `GetArchitecture`,
  `GetEnvironment`, `Reexecute`, `ResetFailedUnit`, `GetUnitFileState`,
  `ListUnitFiles`, `ListUnits`, `ListUnitsFiltered`, `GetUnitByPID`,
  `ListJobs`, `GetJob`, `SetUnitProperties`, `GetUnitProcesses`;
  properties `Version`, `Features`, `Virtualization`, `Architecture`.

**Per unit** (`unit.go`):

- Properties: `Id`, `LoadState`, `ActiveState`, `SubState`, `Description`,
  `UnitFileState`, `Following`, `LoadError` (`org.freedesktop.systemd1.Unit`);
  `StatusText`, `MainPID`, `MemoryCurrent`, `CPUUsageNSec`, `TasksCurrent`
  (`org.freedesktop.systemd1.Service`) - all with `PropertiesChanged`
  emitted on change.
- Methods: `GetUnitFileState`, `Describe`, `Reload`, `Freeze`, `Thaw`.

**Not implemented:** timer/socket/target unit types, `sd_notify` (a
separate protocol from D-Bus - a datagram socket at `$NOTIFY_SOCKET` -
most client libraries no-op if it's unset, which is the common case
here), `ListJobs`/`GetJob` returning anything (every job runs to
completion synchronously and emits `JobRemoved` before the creating call
returns, so nothing is ever pending), persisted unit-file state (see
Limitations), `MainPID` tracking (nothing here execs a process locally
to observe one - hooks run out-of-process - so `GetUnitByPID` never
matches and `GetUnitProcesses` is always empty; `MainPID`/`StatusText`
stay at their zero-value defaults).

## Build

```bash
go mod tidy
CGO_ENABLED=0 GOOS=linux go build -o systemd1-shim .
```

Static binary - `FROM scratch` or `FROM gcr.io/distroless/static` both
work as a base image.

## Hooks

Reacting to a D-Bus unit command is decoupled from the D-Bus/systemd
surface via the `Hook` interface, in its own package (`hooks/hook.go`):

```go
type Hook interface {
	Start(unitName string) error
	Stop(unitName string) error
	Restart(unitName string) error
	Kill(unitName string, signal int) error
}
```

`Manager` (`manager.go`) only ever calls through this interface. A hook
registers itself by name in an `init()`:

```go
func init() {
	register("k8s", func() (Hook, error) { return NewK8sRestarter() })
}
```

Hooks load their own config (env vars, files, whatever they need) - there
is no shared config type. Add a new hook by implementing `Hook` in a new
file under `hooks/` and registering it the same way; nothing in
`manager.go` or `main.go` needs to change.

Which hook runs is picked at invocation time via
[docopt](https://github.com/docopt/docopt-go):

```bash
systemd1-shim --hook=k8s
systemd1-shim --help
```

### Available hooks

- **`noop`** (default) - does nothing beyond logging. No Kubernetes, no
  external dependency of any kind - lets the D-Bus/systemctl-facing
  surface run standalone. `--hook=k8s` is required for it to actually
  restart anything.
- **`k8s`** - signals a sibling container's process directly, by PID
  (found via the Kubernetes API + `/proc`, no exec). See
  [k8s_restarter.md](hooks/k8s_restarter.md).

## Configuration (env vars)

| Var                       | Default            | Purpose                                                                    |
| ------------------------- | ------------------ | -------------------------------------------------------------------------- |
| `DBUS_SYSTEM_BUS_ADDRESS` | _(godbus default)_ | Only needed if your dbus socket isn't at the usual path both sides expect. |

Hook-specific env vars are documented alongside each hook - see
[k8s_restarter.md](hooks/k8s_restarter.md) for the `k8s` hook's.

## Comparison to fakesystemD (Python)

https://github.com/grisuno/fakesystemD

Extends fakesystemD's Manager/Unit surface (see "What's covered" above).
Two functional differences remain:

1. **Signal emission.** fakesystemD doesn't emit `JobNew`/`JobRemoved`,
   so `systemctl` calls through it hang waiting for a signal that never
   comes. This shim emits both for every job-creating call (`runJob()`
   in `unit.go`).
2. **Real actions.** fakesystemD is a pure status mock; this shim's
   hooks *can* actually do something (see "Hooks" above) - the default
   `noop` hook doesn't, but `--hook=k8s` does.

## Limitations

- **State doesn't survive a restart of this shim.** Unit properties
  (mask/enable state, `ActiveState`, ...) are in-memory only (`unit.go`)
  - they reset to healthy/unmasked defaults if the shim restarts.
- **No real job queuing/ordering** - every `StartUnit`/`StopUnit`/
  `RestartUnit` runs synchronously to completion before replying with
  `JobRemoved`. Fine for one-unit-at-a-time blocking CLI calls, not for
  concurrent overlapping job semantics.
- **`ListJobs`/`GetJob` are structurally always empty/not-found** - a
  consequence of jobs completing synchronously, not a missing feature.
