# Running goredis on Kubernetes

The manifests in `deploy/kubernetes/` run one goredis pod with its AOF on a PersistentVolumeClaim.

| File | Purpose | In kustomization |
|------|---------|------------------|
| `namespace.yaml` | Namespace `goredis`, Pod Security `restricted` enforced | yes |
| `configmap.yaml` | Non-secret settings (`GOREDIS_*` env vars) | yes |
| `secret.example.yaml` | Placeholder showing the Secret shape. Never put a real value in it. | **no** |
| `pvc.yaml` | 1Gi ReadWriteOnce volume for `/data` | yes |
| `deployment.yaml` | 1 replica, `Recreate`, probes, security context | yes |
| `service.yaml` | ClusterIP: `resp` 6379, `metrics` 9379, Prometheus scrape annotations | yes |
| `networkpolicy.yaml` | Ingress allow-list | yes |
| `servicemonitor.yaml` | prometheus-operator scrape config, needs the CRD | **no** |
| `kustomization.yaml` | `kubectl apply -k` entry point | |

## Prerequisites

- Kubernetes 1.27 or newer and `kubectl` with built-in kustomize (`kubectl apply -k`).
- A default StorageClass, or set `storageClassName` in `pvc.yaml`.
- A CNI that enforces NetworkPolicy (Calico, Cilium, ...) if you rely on `networkpolicy.yaml`. Without one the policy is accepted and ignored.
- metrics-server for `kubectl top`.
- The image pushed to a registry the cluster can pull from. The release workflow publishes `ghcr.io/<owner>/<repo>`. Replace `ghcr.io/OWNER/goredis:latest` in `deployment.yaml`, or better, set it in `kustomization.yaml`:

  ```yaml
  images:
    - name: ghcr.io/OWNER/goredis
      newName: ghcr.io/your-org/goredis
      newTag: v0.1.0            # or digest: sha256:...
  ```

  Pin a version tag or digest. `latest` makes rollbacks meaningless because the old ReplicaSet points at the same moving tag. If the package is private, add an `imagePullSecrets` entry.

## 1. Create the namespace and the Secret

The password Secret is created out of band so it never sits in git:

```bash
kubectl apply -f deploy/kubernetes/namespace.yaml
kubectl -n goredis create secret generic goredis-auth \
  --from-literal=password="$(openssl rand -base64 32)"
```

Read it back when you need it (for example to connect from your machine):

```bash
kubectl -n goredis get secret goredis-auth -o jsonpath='{.data.password}' | base64 -d
```

The Secret is mounted as a file at `/etc/goredis/auth/password` (mode 0440, group 65532) and the server reads it through `GOREDIS_REQUIREPASS_FILE`. A file is preferred over an env var because env vars leak into `kubectl describe`, crash dumps and child processes more easily. The server reads the file at startup; after rotating the Secret, restart the pod (`kubectl -n goredis rollout restart deployment/goredis`) and update the clients.

In production use a secret manager (External Secrets Operator, Sealed Secrets, SOPS) to create `goredis-auth`.

## 2. Storage class notes

- The PVC requests 1Gi `ReadWriteOnce` from the default StorageClass. The AOF grows with every write until `BGREWRITEAOF` compacts it, and a rewrite temporarily needs room for the old and the new file. Size the volume at 3x the expected compacted AOF at least.
- Use a StorageClass with `allowVolumeExpansion: true` so you can grow the volume in place during a disk-full incident (`kubectl get storageclass` shows the column). See [incidents.md](incidents.md#incident-2-aof-disk-full-and-persistence-write-failures).
- `GOREDIS_AOF_FSYNC=everysec` (the default) tolerates network block storage. `always` fsyncs on every write and is only reasonable on fast local SSDs.
- A `WaitForFirstConsumer` StorageClass binds the volume when the pod is scheduled; the PVC stays `Pending` until then, which is normal.
- `reclaimPolicy: Delete` (common default) deletes the disk when the PVC is deleted. `kubectl delete -k deploy/kubernetes` deletes the PVC. Back up first or patch the PV to `Retain`.

## 3. Apply

```bash
kubectl apply -k deploy/kubernetes          # or: make k8s-apply
kubectl -n goredis rollout status deployment/goredis
kubectl -n goredis get pods,svc,pvc
```

To preview what will be applied: `kubectl kustomize deploy/kubernetes`.

Optional, with prometheus-operator installed:

```bash
kubectl apply -f deploy/kubernetes/servicemonitor.yaml
```

Edit its `release:` label to match your Prometheus `serviceMonitorSelector`.

## 4. Connect with port-forward

```bash
kubectl -n goredis port-forward svc/goredis 6379:6379 9379:9379
redis-cli -a "$(kubectl -n goredis get secret goredis-auth -o jsonpath='{.data.password}' | base64 -d)" PING
curl -s http://127.0.0.1:9379/healthz
curl -s http://127.0.0.1:9379/metrics | grep '^goredis_'
```

`port-forward` goes through the kubelet and is not subject to the NetworkPolicy. In-cluster clients must carry the label (see below).

## 5. Verify the probes

| Probe | Endpoint | Settings | Meaning |
|-------|----------|----------|---------|
| startup | `/readyz` | every 5s, 120 failures allowed (10 min) | Covers the AOF replay at startup. Liveness and readiness start only after it passes. |
| readiness | `/readyz` | every 5s, 2 failures | 200 once the AOF is loaded and the listener accepts. 503 while starting or shutting down, which removes the pod from the Service endpoints. |
| liveness | `/healthz` | every 10s, 3 failures | 200 while the process serves HTTP. Persistence failures do not fail it, because a restart would lose buffered writes. |

```bash
kubectl -n goredis describe pod -l app.kubernetes.io/name=goredis | grep -A3 -E 'Liveness|Readiness|Startup'
kubectl -n goredis get endpointslices -l kubernetes.io/service-name=goredis
kubectl -n goredis get events --field-selector involvedObject.kind=Pod --sort-by=.lastTimestamp
```

If your AOF takes longer than 10 minutes to replay, raise `startupProbe.failureThreshold`. Check `goredis_aof_recovered_commands` and the startup log line to estimate replay time.

## Why `strategy: Recreate`

The AOF is a single-writer file on a ReadWriteOnce volume. With the default `RollingUpdate` (maxSurge 25%), Kubernetes starts the new pod before the old one stops:

- on the same node, both pods would open the same file. The second one fails on the `appendonly.aof.lock` file lock, and without the lock both would append to the file and corrupt it;
- on another node, the new pod hangs in `ContainerCreating` with a `Multi-Attach error` until the old pod releases the volume, and the rollout stalls.

`Recreate` terminates the old pod first. On SIGTERM it stops accepting connections, flushes and fsyncs the AOF and exits (within `GOREDIS_SHUTDOWN_TIMEOUT=20s`, inside `terminationGracePeriodSeconds: 30`). Only then is the new pod created. The cost is a short outage on every rollout, roughly shutdown time plus AOF replay time. Clients should retry with backoff.

Do not raise `replicas`. goredis has no replication; extra pods would not share data.

## Memory: limits, `GOMEMLIMIT` and `GOREDIS_MAXMEMORY`

| Setting | Value | Why |
|---------|-------|-----|
| `resources.limits.memory` | 256Mi | Hard cgroup limit. Crossing it means OOMKilled (exit 137). |
| `GOMEMLIMIT` | 230MiB (about 90% of the limit) | Go's GC normally lets the heap grow to twice the live data (`GOGC=100`) before collecting. Near the limit that overshoot is what triggers the OOM kill. With `GOMEMLIMIT` the GC runs more aggressively as total Go memory approaches 230MiB, which keeps the process under the cgroup limit. The remaining ~26MiB covers memory the soft limit does not fully control. |
| `GOREDIS_MAXMEMORY` | 128mb | Limit on the estimated dataset. Above it SET returns `-OOM`. The estimate does not include map overhead, connection buffers, AOF buffers and GC headroom, so it is set at half the container limit. |

`GOMEMLIMIT` is a soft limit. If the live data alone exceeds it, the GC runs constantly (high CPU) and the pod can still be killed. `GOREDIS_MAXMEMORY` is what bounds the live data. If you change the memory limit, change all three together. See [incidents.md](incidents.md#incident-1-container-oomkilled).

No CPU limit is set on purpose: CFS throttling pauses the whole process for the rest of a 100ms period once its quota is used, which appears as p99 latency spikes and slow garbage collection. The 100m request still guarantees a share under contention. If your cluster requires CPU limits (LimitRange or policy), set a generous one such as 1 CPU.

## Security context

- Pod: `runAsNonRoot`, `runAsUser/runAsGroup 65532`, `fsGroup 65532` (makes the PVC and the Secret volume group-accessible for 65532), `seccompProfile: RuntimeDefault`.
- Container: `readOnlyRootFilesystem`, `allowPrivilegeEscalation: false`, all capabilities dropped. A 16Mi `emptyDir` is mounted at `/tmp`.
- `automountServiceAccountToken: false`: goredis does not talk to the API server.
- The namespace enforces the `restricted` Pod Security Standard, which this Deployment satisfies.

## NetworkPolicy

`networkpolicy.yaml` selects the goredis pod and allows ingress only from:

- port 6379: pods **in the `goredis` namespace** labelled `goredis-client: "true"`;
- port 9379: any pod in the namespace named `monitoring` (Prometheus).

Everything else is dropped, including other pods in the same namespace without the label. Label a client like this:

```yaml
# in the client Deployment's pod template
metadata:
  labels:
    goredis-client: "true"
```

Clients in other namespaces need the alternative rule shown in the file's comments (a `namespaceSelector` and `podSelector` in the same entry). If Prometheus runs in a namespace with a different name, change the `kubernetes.io/metadata.name` value.

Test it:

```bash
# Denied (no label): times out
kubectl -n goredis run np-test --rm -it --restart=Never --image=busybox:1.37 -- \
  sh -c 'nc -w 3 goredis 6379 </dev/null && echo open || echo blocked'
# Allowed
kubectl -n goredis run np-test --rm -it --restart=Never --image=busybox:1.37 \
  --labels=goredis-client=true -- \
  sh -c 'printf "PING\r\n" | nc -w 3 goredis 6379'
```

The second command returns `-NOAUTH Authentication required.` (connection allowed, password still needed). Note the namespace's `restricted` Pod Security level may reject these test pods; add `--overrides` with a compliant security context, or run the test in a namespace with a relaxed policy and the cross-namespace rule.

## Upgrading

1. Change the image tag/digest (in `kustomization.yaml`) or the ConfigMap.
2. `kubectl apply -k deploy/kubernetes`
3. `kubectl -n goredis rollout status deployment/goredis --timeout=10m`

A ConfigMap change alone does not restart the pod. Follow it with `kubectl -n goredis rollout restart deployment/goredis`. Annotate the change so the history is readable:

```bash
kubectl -n goredis annotate deployment/goredis kubernetes.io/change-cause="v0.2.0: fsync everysec" --overwrite
```

## Rolling back

```bash
kubectl -n goredis rollout history deployment/goredis
kubectl -n goredis rollout history deployment/goredis --revision=3
kubectl -n goredis rollout undo deployment/goredis                 # previous revision
kubectl -n goredis rollout undo deployment/goredis --to-revision=3
kubectl -n goredis rollout status deployment/goredis
```

`rollout undo` restores the pod template (image, env, resources). It does **not** restore ConfigMap contents, because the ConfigMap is a separate object referenced by name. Revert ConfigMap changes in git and re-apply. The AOF format is shared across versions; check the release notes before rolling back across a format change.

## Backing up the AOF

The runtime image has no shell or `tar`, so `kubectl cp` (which runs `tar` inside the container) does not work against the goredis container.

Option A: ephemeral debug container sharing the pod's process namespace (needs ephemeral containers, Kubernetes 1.25+, and permission to create them):

```bash
POD=$(kubectl -n goredis get pod -l app.kubernetes.io/name=goredis -o jsonpath='{.items[0].metadata.name}')
kubectl -n goredis debug -it "$POD" --image=busybox:1.37 --target=goredis --profile=restricted -- sh
# inside the debug container, the goredis filesystem is under /proc/1/root:
ls -l /proc/1/root/data/
```

From your machine, stream a copy out through the debug container:

```bash
kubectl -n goredis debug "$POD" --image=busybox:1.37 --target=goredis --profile=restricted \
  --container=backup -- sleep 600
kubectl -n goredis exec "$POD" -c backup -- cat /proc/1/root/data/appendonly.aof > appendonly-$(date +%F).aof
```

Reading `/proc/1/root` requires the debug container to run as the same user as the goredis process. The pod-level `runAsUser: 65532` applies to ephemeral containers too, and `--profile=restricted` keeps the debug container compliant with the namespace's `restricted` Pod Security level (the default profile may be rejected).

Option B: a VolumeSnapshot of the PVC, if your CSI driver supports snapshots. This is the most consistent option.

Option C: briefly stop the server (`kubectl -n goredis scale deployment/goredis --replicas=0`), mount the PVC in a helper pod, copy the file, scale back to 1. This gives a consistent copy at the cost of downtime.

The AOF is append-only, so a copy taken while the server runs is a valid prefix of the log at worst, possibly with a truncated last record. Validate any backup:

```bash
goredis-check-aof appendonly-2026-11-12.aof
```

Run `BGREWRITEAOF` before a backup to make the file smaller.
