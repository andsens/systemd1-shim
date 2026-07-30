# systemd1-shim

**Disclaimer: This is completely vibe-coded with Claude AI, so do not expected exceptional code-quality**

A fake `org.freedesktop.systemd1` D-Bus service, so `unifi-core` **and the
real `systemctl` binary already in its image** both work against a shared
D-Bus socket with no actual systemd anywhere in the pod. Restart/stop/kill
are implemented by execing `/sbin/killall5` into the sibling container in
the same pod, via the Kubernetes API's pod/exec subresource directly
(`k8s.io/client-go`) - no `kubectl` binary or subprocess involved.

> **I could not compile or run this in the sandbox that wrote it** - no Go
> toolchain, no network to fetch dependencies. The D-Bus method/property/
> signal names and shapes are accurate (they're systemd's own stable
> D-Bus API), and the godbus/client-go usage follows those libraries'
> documented patterns as I know them, but treat `go build` as your first
> step, not an afterthought - expect to fix minor drift, particularly in
> godbus's `prop` package API and in exactly which client-go version's
> `remotecommand` package has `StreamWithContext` vs plain `Stream`.

## What's covered

**D-Bus, `org.freedesktop.systemd1.Manager`:**

- Core surface `unifi-core` and real `systemctl` actually call:
  `Subscribe`, `Unsubscribe`, `LoadUnit`, `GetUnit`, `StartUnit`,
  `StopUnit`, `RestartUnit`, `KillUnit`, `EnableUnitFiles`,
  `DisableUnitFiles`, `MaskUnitFiles`, `UnmaskUnitFiles`, `Reload`;
  property `SystemState` (→ `systemctl is-system-running`); signals
  `JobNew`/`JobRemoved` (so `systemctl` unblocks after `enable --now`/
  `disable --now`/`restart`/`mask --now` instead of hanging).
- Broader surface added for parity with the `fakesystemD` reference
  project (`manager_extra.go`): `GetVersion`, `GetFeatures`,
  `GetVirtualization`, `GetArchitecture`, `GetEnvironment`, `Reexecute`,
  `ResetFailedUnit`, `GetUnitFileState`, `ListUnitFiles`, `ListUnits`,
  `ListUnitsFiltered`, `GetUnitByPID`, `ListJobs`, `GetJob`,
  `SetUnitProperties`, `GetUnitProcesses`; properties `Version`,
  `Features`, `Virtualization`, `Architecture`.

**Per unit** (`unit.go` + `unit_extra.go`):

- Properties: `Id`, `LoadState`, `ActiveState`, `SubState`, `Description`,
  `UnitFileState`, `Following`, `LoadError` (`org.freedesktop.systemd1.Unit`);
  `StatusText`, `MainPID`, `MemoryCurrent`, `CPUUsageNSec`, `TasksCurrent`
  (`org.freedesktop.systemd1.Service`) - all with `PropertiesChanged`
  emitted on change.
- Methods: `GetUnitFileState`, `Describe`, `Reload`, `Freeze`, `Thaw`.

**Not implemented:** timer/socket/target unit types, `sd_notify` (see
below), `ListJobs`/`GetJob` returning anything (every job we create runs
to completion and emits `JobRemoved` before the creating method call even
returns - see `unit.go` - so there's structurally nothing left pending by
the time anything could list it), actual persisted unit-file state
(mask/enable state resets if this sidecar restarts - see Limitations).

**`sd_notify`:** `unifi-core`'s own bootstrap has an `sdnotify` subsystem,
which is a _different_ protocol from D-Bus (a Unix datagram socket at
`$NOTIFY_SOCKET`, used for `READY=1`/`WATCHDOG=1`/etc). This shim doesn't
implement it. Almost every `sd_notify` client library no-ops if
`$NOTIFY_SOCKET` isn't set, and nothing in this setup sets it, so it's
likely a non-issue - but it's the one integration point neither of us has
actually confirmed. If it turns out to matter, keep an eye on `errors.log`
for it and treat it as a separate small UDP listener to add, same shape as
`fakesystemD`'s `NotifyListener` (its `docs`/full source covers the
`SCM_RIGHTS`/`FDSTORE` handling if you need that level of fidelity).

## Build

```bash
go mod tidy      # fetches godbus + k8s.io/{api,apimachinery,client-go}
CGO_ENABLED=0 GOOS=linux go build -o systemd1-shim .
```

A static binary - `FROM scratch` or `FROM gcr.io/distroless/static` both
work as the sidecar's base image. No `kubectl` binary needed inside it -
`k8s_restart.go` talks to the Kubernetes API directly.

(`exec_restart.go`'s `KubectlRestarter`, which does shell out to
`kubectl`, is kept in the tree as a documented fallback if you'd rather
not vendor `client-go` - swap `NewK8sRestarter(cfg)` for
`NewKubectlRestarter(cfg)` in `main.go` if so. Not used by default.)

## Configuration (env vars)

| Var                       | Default            | Purpose                                                                                                                                                                                                             |
| ------------------------- | ------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `POD_NAME`                | _(required)_       | Own pod name - target for the exec call. Wire via Downward API, not hardcoded.                                                                                                                                      |
| `POD_NAMESPACE`           | `default`          | Namespace for the exec call. Also via Downward API.                                                                                                                                                                 |
| `EXEC_TIMEOUT_SECONDS`    | `30`               | Timeout per exec call.                                                                                                                                                                                              |
| `UNIT_CONTAINER_MAP_FILE` | _(none)_           | Path to a JSON file `{"unifi-network.service": "network-app", ...}` for units whose container name doesn't match the unit name. Anything not listed falls back to unit-name-minus-`.service` as the container name. |
| `DBUS_SYSTEM_BUS_ADDRESS` | _(godbus default)_ | Only needed if your dbus socket isn't at the usual path both sides expect.                                                                                                                                          |

`k8s_restart.go` uses `rest.InClusterConfig()` - it expects to actually be
running in a pod with a ServiceAccount token mounted (the normal
Kubernetes-provided one, nothing extra to configure). No kubeconfig, no
separate credentials.

## Sidecar spec (sketch)

```yaml
spec:
  serviceAccountName: unifi-core-sa # <- needs the RBAC below
  containers:
    - name: unifi-core
      image: your-unifi-core-image
      volumeMounts:
        - name: dbus-socket
          mountPath: /var/run/dbus

    - name: unifi-network # <- whatever your other sidecars are
      image: your-unifi-network-image
      # ...

    - name: systemd1-shim
      image: your-systemd1-shim-image
      env:
        - name: POD_NAME
          valueFrom: { fieldRef: { fieldPath: metadata.name } }
        - name: POD_NAMESPACE
          valueFrom: { fieldRef: { fieldPath: metadata.namespace } }
      volumeMounts:
        - name: dbus-socket
          mountPath: /var/run/dbus

  volumes:
    - name: dbus-socket
      emptyDir: {}
```

You still need an actual `dbus-daemon` publishing the system bus socket
into that shared `emptyDir` — this shim is a service _on_ the bus, not the
bus itself (same as before: it connects to `/var/run/dbus/system_bus_socket`
just like `unifi-core`'s own D-Bus client does).

## RBAC

The shim needs to `exec` into its own pod via the Kubernetes API - scope
this as tightly as your cluster allows, e.g. restricted to the pod's own
name via `resourceNames` if you template that per-pod, or namespace-wide
`pods/exec` if simpler:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: systemd1-shim-exec
rules:
  - apiGroups: [""]
    resources: ["pods/exec"]
    verbs: ["create"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: systemd1-shim-exec
subjects:
  - kind: ServiceAccount
    name: unifi-core-sa # match spec.serviceAccountName above
roleRef:
  kind: Role
  name: systemd1-shim-exec
  apiGroup: rbac.authorization.k8s.io
```

## Comparison to fakesystemD (Python)

Extended to match its broader Manager/Unit method and property surface
(see "What's covered" above) since you pointed at it as a reference. The
two real functional differences remain:

1. **Signal emission.** fakesystemD's own limitations note it doesn't
   emit `JobNew`/`JobRemoved` - real `systemctl` blocks waiting for
   `JobRemoved` by default, so calls through it would hang/time out
   rather than complete. This shim emits both for every job-creating
   call (`runJob()` in `unit.go`).
2. **Real actions.** fakesystemD explicitly does not execute external
   commands - it's a pure status mock. This shim's whole reason for
   existing is the opposite: making `systemctl restart` actually restart
   something, via the Kubernetes API.

## Limitations

- **State doesn't survive a restart of this sidecar.** Mask/enable state
  and last-known property values are all in memory. If the shim restarts,
  every unit comes back healthy/unmasked until `unifi-core` or something
  else touches it again. If you need this to survive restarts, `config.go`
  is the natural place to add a similar load/save for unit state (a small
  JSON file on a persistent volume).
- **`killall5 -15` is a blunt instrument** — it signals every process in
  the target container except PID 1. Fine if that container runs exactly
  one supervised service; less fine if it runs several. Adjust
  `k8s_restart.go`'s `Restart`/`Stop`/`Kill` if you need more precision
  (e.g. a specific process name, or exec'ing a supervisor CLI instead).
- **No real job queuing/ordering** — every `StartUnit`/`StopUnit`/
  `RestartUnit` runs synchronously to completion before replying with
  `JobRemoved`. Real systemd can queue and reorder jobs; this can't. Fine
  for the actual call pattern here (one unit at a time, blocking CLI
  calls), not fine if you ever need concurrent overlapping job semantics.
- **`ListJobs`/`GetJob` are structurally always empty/not-found** - see
  "What's covered" above; this is a consequence of jobs completing
  synchronously, not a missing feature.
