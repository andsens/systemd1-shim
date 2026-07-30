# The `k8s` hook

`k8s_restarter.go`'s `K8sRestarter` is the [Hook](../README.md#hooks) that
makes `systemctl restart` actually restart something - select it with
`--hook=k8s` (the default is `noop`, which does nothing). It signals a
sibling container's own main process directly, by PID - no exec, no
subprocess, no tools required inside the target container at all.

`Restart`/`Stop`/`Kill` send `SIGTERM` (or, for `Kill`, whatever signal
`systemctl kill` asked for) straight to that PID via `syscall.Kill`. The
one remaining use of the Kubernetes API is read-only: `containerIDFor`
fetches the pod's `ContainerStatuses` to get the target container's
current container ID, and `pidForContainer` finds the matching PID by
scanning `/proc/*/cgroup` for that ID - the standard way to correlate a
container to a PID from inside a shared PID namespace. That's _why_ this
works at all: the pod needs `shareProcessNamespace: true` (see
[the example deployment](../example/k8s_restarter_deployment.yaml)) so
this container can see - and signal - processes belonging to its
siblings. `Start` is a no-op - Kubernetes already keeps containers
running per their `restartPolicy`, so there's no generic "start it from
outside" primitive; if the container is genuinely down, the next real
action surfaces a failure instead of a fake success.

Requires in-cluster credentials: `NewK8sRestarter` uses
`rest.InClusterConfig()`, so it expects to actually run in a pod with a
ServiceAccount token mounted (the normal Kubernetes-provided one, nothing
extra to configure). No kubeconfig, no separate credentials.

## Configuration (env vars)

| Var                            | Default      | Purpose                                                                                                                                                                                                                                                        |
| ------------------------------ | ------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `POD_NAME`                     | _(required)_ | Own pod name - which pod's `ContainerStatuses` to read. Wire via the Downward API, not hardcoded.                                                                                                                                                              |
| `POD_NAMESPACE`                | `default`    | Namespace for that lookup. Also via the Downward API.                                                                                                                                                                                                          |
| `SD1_SHIM_K8S_MAP_<UNIT_NAME>` | _(none)_     | One per unit whose container name doesn't match. `<UNIT_NAME>` is the unit name (suffix stripped, uppercased, `-` turned into `_`); the value is the container name. Units with no matching var fall back to unit-name-minus-`.service` as the container name. |

```yaml
env:
  - name: SD1_SHIM_K8S_MAP_MY_APP
    value: app-container
```

## RBAC

The shim only needs read access to its own pod - `get` on `pods`, to
read `ContainerStatuses`. See
[example/k8s_restarter_sa.yaml](../example/k8s_restarter_sa.yaml)
for a ready-to-apply ServiceAccount/Role/RoleBinding - point
`spec.serviceAccountName` at its ServiceAccount. Scope the Role as
tightly as your cluster allows, e.g. restricted to the pod's own name
via `resourceNames` if you template that per-pod (see the commented-out
line in the manifest), or namespace-wide `pods` `get` if simpler. Nothing
here can execute code in another container or inject one - this is a
strictly smaller grant than the `pods/exec` this hook used to need.

## Limitations

- **Depends on `shareProcessNamespace: true`.** Without it, this
  container's PID namespace is isolated from its siblings and
  `pidForContainer` will never find a match - `Restart`/`Stop`/`Kill`
  fail cleanly with an error naming this as the likely cause, rather than
  silently doing nothing.
