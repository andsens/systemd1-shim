# The `k8s` hook

`k8s_restart.go`'s `K8sRestarter` is the default [Hook](README.md#hooks).
It execs into a sibling container via the Kubernetes API's pod/exec
subresource directly (`k8s.io/client-go`) - the client-go equivalent of
`kubectl exec`, no `kubectl` binary or subprocess needed.

`Restart`/`Stop`/`Kill` send `/sbin/killall5` (sysvinit-utils) into the
target container. `Start` is a no-op - Kubernetes already keeps
containers running per their `restartPolicy`, so there's no generic
"start it from outside" primitive; if the container is genuinely down,
the next real action surfaces a failure instead of a fake success.

Requires in-cluster credentials: `NewK8sRestarter` uses
`rest.InClusterConfig()`, so it expects to actually run in a pod with a
ServiceAccount token mounted (the normal Kubernetes-provided one, nothing
extra to configure). No kubeconfig, no separate credentials.

## Configuration (env vars)

| Var                       | Default       | Purpose                                                                                                                                                                                                             |
| ------------------------- | ------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `POD_NAME`                | _(required)_  | Own pod name - target for the exec call. Wire via the Downward API, not hardcoded.                                                                                                                                  |
| `POD_NAMESPACE`           | `default`     | Namespace for the exec call. Also via the Downward API.                                                                                                                                                             |
| `EXEC_TIMEOUT_SECONDS`    | `30`          | Timeout per exec call.                                                                                                                                                                                              |
| `UNIT_CONTAINER_MAP_FILE` | _(none)_      | Path to a JSON file `{"my-app.service": "app-container", ...}` for units whose container name doesn't match the unit name. Anything not listed falls back to unit-name-minus-`.service` as the container name. |

## Sidecar spec (sketch)

```yaml
spec:
  serviceAccountName: my-workload-sa # <- needs the RBAC below
  containers:
    - name: my-app
      image: your-app-image
      volumeMounts:
        - name: dbus-socket
          mountPath: /var/run/dbus

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

An actual `dbus-daemon` still needs to publish the system bus socket into
that shared `emptyDir` - this shim is a service _on_ the bus, not the bus
itself. It connects to `/var/run/dbus/system_bus_socket` like any other
D-Bus client.

## RBAC

The shim needs to `exec` into its own pod via the Kubernetes API. See
[k8s_restart_sa.yaml](k8s_restart_sa.yaml) for a ready-to-apply
ServiceAccount/Role/RoleBinding - point `spec.serviceAccountName` at its
ServiceAccount. Scope the Role as tightly as your cluster allows, e.g.
restricted to the pod's own name via `resourceNames` if you template
that per-pod (see the commented-out line in the manifest), or
namespace-wide `pods/exec` if simpler.

## Limitations

- **`killall5 -15` is a blunt instrument** - it signals every process in
  the target container except PID 1. Fine if that container runs exactly
  one supervised service; less fine if it runs several. Adjust
  `Restart`/`Stop`/`Kill` in `k8s_restart.go` if you need more precision
  (e.g. a specific process name, or exec'ing a supervisor CLI instead).
