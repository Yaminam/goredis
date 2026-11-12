# Incidents and runbooks

This page has two parts:

1. [Alert runbooks](#alert-runbooks): one section per alert in `monitoring/prometheus/alerts.yml`. Each alert's `runbook` annotation links here.
2. Three incident simulations, written as post-incident reviews, each with a safe way to reproduce it:
   - [Incident 1: Container OOMKilled](#incident-1-container-oomkilled)
   - [Incident 2: AOF disk full and persistence write failures](#incident-2-aof-disk-full-and-persistence-write-failures)
   - [Incident 3: A bad release raises latency or errors](#incident-3-a-bad-release-raises-latency-or-errors)

Conventions used in the commands:

```bash
NS=goredis
POD=$(kubectl -n $NS get pod -l app.kubernetes.io/name=goredis -o jsonpath='{.items[0].metadata.name}')
PW=$(kubectl -n $NS get secret goredis-auth -o jsonpath='{.data.password}' | base64 -d)
# For metrics and redis-cli from your machine:
kubectl -n $NS port-forward svc/goredis 6379:6379 9379:9379 &
```

Never run a simulation against a production instance. Use a kind/minikube cluster, a separate namespace, or a local Docker container.

---

## Alert runbooks

### GoredisDown

Prometheus cannot scrape the admin port for 1 minute.

1. Is the pod running? `kubectl -n $NS get pod -l app.kubernetes.io/name=goredis -o wide`
2. Restarting or crash-looping: `kubectl -n $NS describe pod $POD` (look at `Last State` and the exit code: 137 = OOMKilled or SIGKILL, see [Incident 1](#incident-1-container-oomkilled)) and `kubectl -n $NS logs $POD --previous`.
3. Refused to start: the logs say why. Common causes are protected mode without a password, a corrupt AOF, the AOF lock held by another process, or `permission denied` on `/data`. See [troubleshooting.md](troubleshooting.md).
4. Running but not scraped: check the NetworkPolicy admits the Prometheus namespace on 9379 and that the target appears on the Prometheus "Targets" page.

### GoredisHighErrorRate

More than 5% of commands return an error for 5 minutes.

1. Which command and which error? `sum by (command) (rate(goredis_command_errors_total[5m]))`, then look at the server logs.
2. `MISCONF` errors on writes: persistence is failing, see [Incident 2](#incident-2-aof-disk-full-and-persistence-write-failures).
3. `OOM` errors on SET: the dataset reached `maxmemory`, see [GoredisMemoryNearLimit](#goredismemorynearlimit).
4. `NOAUTH`/`WRONGPASS`: a client has the wrong password, often after a Secret rotation.
5. `command ... is disabled` or a spike right after a deploy: see [Incident 3](#incident-3-a-bad-release-raises-latency-or-errors).

### GoredisHighLatencyP99

Server-side p99 command latency above 10ms for 10 minutes.

1. Which commands? `topk(5, histogram_quantile(0.99, sum by (command, le) (rate(goredis_command_duration_seconds_bucket[5m]))))`
2. Disk: `histogram_quantile(0.99, sum by (le) (rate(goredis_aof_fsync_duration_seconds_bucket[5m])))`. Slow fsync with `GOREDIS_AOF_FSYNC=always` blocks writes.
3. CPU: `kubectl -n $NS top pod`. Constant GC near `GOMEMLIMIT` or CPU starvation on the node raises latency for every command.
4. Load: command rate and connected clients on the dashboard. Compare with the previous week.
5. Just deployed? See [Incident 3](#incident-3-a-bad-release-raises-latency-or-errors).

### GoredisAOFUnhealthy

`goredis_aof_healthy == 0` for 1 minute. Write commands are being rejected with `-MISCONF`; reads still work. Follow [Incident 2](#incident-2-aof-disk-full-and-persistence-write-failures). Do not restart the pod as a first step: buffered writes that have not reached the disk would be lost.

### GoredisAOFWriteErrors

`goredis_aof_write_errors_total` or `goredis_aof_fsync_errors_total` increased in the last 5 minutes. Usually fires together with GoredisAOFUnhealthy. A single write error that the retry fixed is still worth a look at disk usage. An fsync error keeps the AOF unhealthy until a successful `BGREWRITEAOF` or a restart. See [Incident 2](#incident-2-aof-disk-full-and-persistence-write-failures).

### GoredisMemoryNearLimit

The estimated dataset is above 90% of `maxmemory`. At 100%, SET returns `-OOM command not allowed when used memory > 'maxmemory'.` (no eviction).

1. Is this growth expected? Look at `goredis_keys` and the command rate by command.
2. Keys without TTL accumulating: compare `rate(goredis_expired_keys_total[1h])` with the SET rate.
3. Options: delete or expire data on the client side, or raise `GOREDIS_MAXMEMORY` together with the container memory limit and `GOMEMLIMIT` (see [kubernetes.md](kubernetes.md#memory-limits-gomemlimit-and-goredis_maxmemory)).

The Kubernetes variant (container working set vs. memory limit) is the early warning for [Incident 1](#incident-1-container-oomkilled).

### GoredisTooManyClients

More than 800 connected clients for 5 minutes, or connections were rejected with `-ERR max number of clients reached`.

1. Connection leak? A steady climb in `goredis_connected_clients` while the command rate is flat usually means a client opens connections and never closes them. Idle connections are closed after `GOREDIS_IDLE_TIMEOUT` (5m).
2. New client deployment with a large pool? Check which workloads have the `goredis-client` label and their pool sizes.
3. Raise `GOREDIS_MAX_CLIENTS` only after confirming the load is legitimate; each connection costs a goroutine and buffers.

### GoredisRecentRestart

Uptime is under 5 minutes. Expected after a deploy. Otherwise find out why it restarted:

```bash
kubectl -n $NS get pod $POD -o jsonpath='{.status.containerStatuses[0].lastState}'
kubectl -n $NS logs $POD --previous | tail -n 50
```

Check `goredis_aof_truncated_bytes`: a non-zero value means the previous process ended without a clean shutdown and an incomplete last record was dropped.

---

## Incident 1: Container OOMKilled

### Symptoms

- Clients see connection resets and then `connection refused` for some seconds, repeatedly.
- The pod's restart count rises. `GoredisRecentRestart` fires; `GoredisDown` may fire.
- Memory on the dashboard climbs in a sawtooth that ends at the container limit each time.

### Detection

- Alerts: `GoredisRecentRestart`, then the Kubernetes memory alert (`container_memory_working_set_bytes / kube_pod_container_resource_limits > 0.9`, see `alerts.yml`). `GoredisMemoryNearLimit` does not fire, because `maxmemory` was 0 (unlimited).
- Dashboard: "Memory" panel (RSS and heap in use reach the limit, the estimated dataset line keeps growing, no maxmemory line), "Uptime" resets.

### Diagnosis

```bash
kubectl -n $NS get pod -l app.kubernetes.io/name=goredis
# RESTARTS > 0

kubectl -n $NS describe pod $POD
# Last State:  Terminated
#   Reason:    OOMKilled
#   Exit Code: 137

kubectl -n $NS get events --sort-by=.lastTimestamp | tail -n 20
# BackOff / Killing / Started; on some nodes also an OOMKilling event from the node

kubectl -n $NS top pod $POD --containers
# MEMORY close to the 256Mi limit

kubectl -n $NS get deployment goredis -o jsonpath='{.spec.template.spec.containers[0].resources}'
kubectl -n $NS get configmap goredis-config -o jsonpath='{.data.GOREDIS_MAXMEMORY}'
kubectl -n $NS get deployment goredis -o jsonpath='{.spec.template.spec.containers[0].env}'
# maxmemory "0" and no GOMEMLIMIT
```

Exit code 137 is 128 + 9 (SIGKILL). The kernel OOM killer does not send SIGTERM, so there was no graceful shutdown: writes since the last fsync (up to 1 second with `everysec`) are lost, and the next start may truncate an incomplete record (`goredis_aof_truncated_bytes > 0`).

### Mitigation

1. Stop the crash loop by giving the process room and a bound:

   ```bash
   kubectl -n $NS patch configmap goredis-config --type merge \
     -p '{"data":{"GOREDIS_MAXMEMORY":"128mb"}}'
   kubectl -n $NS set env deployment/goredis GOMEMLIMIT=230MiB
   kubectl -n $NS set resources deployment/goredis --limits=memory=256Mi
   kubectl -n $NS rollout restart deployment/goredis
   kubectl -n $NS rollout status deployment/goredis
   ```

   If the dataset on disk is already larger than the new limit allows, the server cannot replay the AOF within the limit. Raise the memory limit temporarily (and `GOMEMLIMIT` to ~90% of it), let it start, then reduce the data.

2. Tell the client owners that SET now returns `-OOM` above 128mb instead of the server dying.

### Root cause

`GOREDIS_MAXMEMORY` was unset (0 = unlimited) and `GOMEMLIMIT` was not set. The dataset kept growing. With the default `GOGC=100`, the Go runtime lets the heap grow to about twice the live data before the next collection, so the process needed roughly 2x the live heap plus non-heap memory. That crossed the 256Mi cgroup limit and the kernel killed the container. Nothing in the process knew where the limit was.

### Prevention

- `GOREDIS_MAXMEMORY` set well below the container limit (128mb for 256Mi). The dataset is bounded and clients get a clear `-OOM` error.
- `GOMEMLIMIT` at about 90% of the container limit, so the GC works harder near the limit instead of overshooting.
- Alerts: `GoredisMemoryNearLimit` (dataset vs. maxmemory) and the Kubernetes working-set variant (container vs. limit).
- Keep these three values together in review: the memory limit, `GOMEMLIMIT` and `GOREDIS_MAXMEMORY`.

### Verification

```bash
kubectl -n $NS describe pod $POD | grep -A3 'Last State'     # no new OOMKilled
kubectl -n $NS get pod $POD -w                               # RESTARTS stays constant
```

Rerun the load from the simulation below: the pod stays up, memory flattens below the limit, and the bench starts to report `OOM command not allowed when used memory > 'maxmemory'.` errors. On the dashboard, the estimated dataset line stops at the maxmemory line.

### How to simulate it safely

Locally with Docker (fastest):

```bash
docker run -d --name goredis-oom --memory=64m --memory-swap=64m \
  -e GOREDIS_PROTECTED_MODE=false -e GOREDIS_MAXMEMORY=0 \
  -p 127.0.0.1:6379:6379 -p 127.0.0.1:9379:9379 goredis:dev

# Large values, large key space: memory grows until the kernel kills the container.
docker run --rm --network host --entrypoint goredis-bench goredis:dev \
  -addr 127.0.0.1:6379 -clients 8 -requests 200000 -keyspace 200000 -value-size 4096

docker inspect --format 'OOMKilled={{.State.OOMKilled}} ExitCode={{.State.ExitCode}}' goredis-oom
# OOMKilled=true ExitCode=137

# Fixed: same limit, bounded dataset and GC target
docker rm -f goredis-oom
docker run -d --name goredis-oom --memory=64m --memory-swap=64m \
  -e GOREDIS_PROTECTED_MODE=false -e GOREDIS_MAXMEMORY=24mb -e GOMEMLIMIT=57MiB \
  -p 127.0.0.1:6379:6379 -p 127.0.0.1:9379:9379 goredis:dev
# Rerun the bench: it reports OOM errors, the container stays up.
docker rm -f goredis-oom
```

On Docker Desktop, replace `--network host` and `127.0.0.1` with `-addr host.docker.internal:6379`. Use `-ratio` to make the run write-heavy (see `goredis-bench -h` for its format).

In a test cluster:

```bash
# 1. Make the deployment fragile
kubectl -n $NS patch configmap goredis-config --type merge -p '{"data":{"GOREDIS_MAXMEMORY":"0"}}'
kubectl -n $NS set env deployment/goredis GOMEMLIMIT-
kubectl -n $NS set resources deployment/goredis --limits=memory=64Mi
kubectl -n $NS rollout status deployment/goredis

# 2. Generate load from inside the cluster (labelled, so the NetworkPolicy admits it)
cat <<'EOF' | kubectl -n $NS apply -f -
apiVersion: batch/v1
kind: Job
metadata:
  name: goredis-bench
spec:
  backoffLimit: 0
  template:
    metadata:
      labels:
        goredis-client: "true"
    spec:
      restartPolicy: Never
      automountServiceAccountToken: false
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: bench
          image: ghcr.io/OWNER/goredis:latest   # same image as the server
          command: ["goredis-bench"]
          args: ["-addr", "goredis:6379", "-password", "$(GOREDIS_PASSWORD)",
                 "-clients", "16", "-requests", "500000",
                 "-keyspace", "500000", "-value-size", "4096"]
          env:
            - name: GOREDIS_PASSWORD
              valueFrom:
                secretKeyRef:
                  name: goredis-auth
                  key: password
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop: ["ALL"]
EOF

# 3. Watch it die, then diagnose as above
kubectl -n $NS get pod -l app.kubernetes.io/name=goredis -w

# 4. Clean up and restore the manifests from git
kubectl -n $NS delete job goredis-bench
kubectl apply -k deploy/kubernetes
kubectl -n $NS rollout restart deployment/goredis
```

---

## Incident 2: AOF disk full and persistence write failures

### Symptoms

- Every write command (SET, DEL, EXPIRE, ...) fails with `-MISCONF Errors writing to the AOF file: ...` (the message includes the OS error, for example `no space left on device`).
- Reads (GET, TTL, EXISTS) keep working.
- The pod stays `Running` and `Ready`. Liveness stays green on purpose: restarting would throw away the writes buffered in memory that have not reached the disk yet.

### Detection

- Alerts: `GoredisAOFUnhealthy` (critical, after 1m), `GoredisAOFWriteErrors`, and `GoredisHighErrorRate` as clients retry.
- Metrics: `goredis_aof_healthy 0`, `goredis_aof_write_errors_total` rising about once per second (one per retry), `goredis_command_errors_total` rising for SET and the other write commands, `goredis_aof_size_bytes` flat at its maximum.
- Dashboard: "AOF healthy" shows FAILING (red); "AOF write / fsync errors" and "Error rate (%)" climb.
- `/healthz` JSON shows `"aof_healthy": false`.

### Diagnosis

```bash
curl -s http://127.0.0.1:9379/healthz
curl -s http://127.0.0.1:9379/metrics | grep -E '^goredis_aof_(healthy|write_errors_total|fsync_errors_total|size_bytes)'
kubectl -n $NS logs $POD --since=10m | grep -i aof | tail -n 20
# e.g. "write /data/appendonly.aof: no space left on device"

# Volume capacity and usage (the image has no df; use the kubelet stats or a debug container)
kubectl -n $NS get pvc goredis-data
kubectl -n $NS debug -it $POD --image=busybox:1.37 --target=goredis --profile=restricted -- df -h /proc/1/root/data
# With kubelet volume metrics in Prometheus:
#   kubelet_volume_stats_used_bytes{persistentvolumeclaim="goredis-data"}
#   / kubelet_volume_stats_capacity_bytes{persistentvolumeclaim="goredis-data"}
```

Distinguish the two failure modes:

- **Write errors** (`goredis_aof_write_errors_total` rising, fsync errors flat): the data could not be written, typically ENOSPC. The server keeps the data in its buffer and retries every second. Once a retry succeeds, writes resume by themselves.
- **Fsync errors** (`goredis_aof_fsync_errors_total` increased): the kernel accepted the write but could not persist it. After a failed fsync, the kernel may already have dropped the dirty pages, so the file content cannot be trusted. This state is sticky: the AOF stays unhealthy until a successful `BGREWRITEAOF` (which writes a fresh file from memory) or a restart.

### Mitigation

1. Make room on the volume. Preferred: expand the PVC, if the StorageClass allows it:

   ```bash
   kubectl get storageclass "$(kubectl -n $NS get pvc goredis-data -o jsonpath='{.spec.storageClassName}')" \
     -o jsonpath='{.allowVolumeExpansion}'
   # true -> expand:
   kubectl -n $NS patch pvc goredis-data -p '{"spec":{"resources":{"requests":{"storage":"5Gi"}}}}'
   kubectl -n $NS get pvc goredis-data -w
   kubectl -n $NS describe pvc goredis-data   # FileSystemResizePending / resize finished
   ```

   Most CSI drivers grow the filesystem online. Some need the pod restarted to finish the filesystem resize (the PVC condition says `FileSystemResizePending`). In that case, wait until the AOF is healthy or accept the loss of the buffered writes before restarting.

   If expansion is not possible, delete something else on the volume (a stale `*.bak` from a previous `goredis-check-aof -fix`, for example) through a debug container. Never delete or truncate `appendonly.aof` itself.

2. Wait for the next retry (at most about a second). Check `goredis_aof_healthy` returns to 1 and writes succeed:

   ```bash
   redis-cli -a "$PW" SET incident:probe ok
   ```

3. Compact the AOF so the same data takes less space, and so an fsync failure is cleared:

   ```bash
   redis-cli -a "$PW" BGREWRITEAOF
   curl -s http://127.0.0.1:9379/metrics | grep -E '^goredis_aof_(rewrites_total|size_bytes|healthy)'
   ```

   A rewrite needs free space for the new file while the old one still exists, so do this after step 1. A rewrite that fails increments `goredis_aof_rewrites_total{result="failure"}`.

4. If the AOF stays unhealthy only because of a past fsync error and `BGREWRITEAOF` succeeds, the incident is over. If it does not succeed, a restart reloads from the file on disk; anything that was only in the failed region is lost, so record the time window for the client owners.

### Root cause

The AOF grows with every write and is only compacted by `BGREWRITEAOF`. The volume was sized for the dataset, not for the uncompacted log, and nothing compacted it or alerted on volume usage, so it reached 100% and every append failed with ENOSPC.

### Prevention

- Size the PVC at 3x the expected compacted AOF, and use a StorageClass with `allowVolumeExpansion: true`.
- Alert on volume usage before it is full: `kubelet_volume_stats_used_bytes / kubelet_volume_stats_capacity_bytes > 0.8`.
- Run `BGREWRITEAOF` regularly (for example a nightly CronJob that runs `redis-cli BGREWRITEAOF`), and alert on `increase(goredis_aof_rewrites_total{result="failure"}[1d]) > 0`.
- Keep `GoredisAOFUnhealthy` as a paging alert; writes are failing while it fires.

### Verification

```bash
curl -s http://127.0.0.1:9379/healthz                          # "aof_healthy": true
curl -s http://127.0.0.1:9379/metrics | grep '^goredis_aof_healthy'   # 1
redis-cli -a "$PW" SET incident:verify ok && redis-cli -a "$PW" GET incident:verify
```

Then validate the file itself. `goredis-check-aof` without `-fix` only reads, but run it against a copy or a stopped server to avoid racing with appends:

```bash
# Docker: stop the server, check the file on its volume, start it again
docker stop -t 30 goredis
docker run --rm -v goredis-data:/data --entrypoint goredis-check-aof goredis:dev /data/appendonly.aof
docker start goredis

# Kubernetes: check a backup copy (see kubernetes.md "Backing up the AOF")
goredis-check-aof appendonly-backup.aof
```

The dashboard shows "AOF healthy" green, the error rate back to its baseline and the AOF size dropping after the rewrite.

### How to simulate it safely

Option A, Docker with a 1 MB tmpfs (quick; shows symptoms and detection). The `tmpfs-mode=1777` is needed because the server runs as uid 65532 and a fresh tmpfs is owned by root:

```bash
docker run -d --name goredis-diskfull \
  -e GOREDIS_PROTECTED_MODE=false \
  --mount type=tmpfs,destination=/data,tmpfs-size=1m,tmpfs-mode=1777 \
  -p 127.0.0.1:6379:6379 -p 127.0.0.1:9379:9379 goredis:dev

# Write more than 1 MB
docker run --rm --network host --entrypoint goredis-bench goredis:dev \
  -addr 127.0.0.1:6379 -clients 4 -requests 20000 -keyspace 100 -value-size 1024

redis-cli SET k v                       # (error) MISCONF Errors writing to the AOF file: ...
redis-cli GET k                         # reads still work
curl -s http://127.0.0.1:9379/metrics | grep -E '^goredis_aof_(healthy|write_errors_total)'

# "Expand the disk": grow the tmpfs inside the container's mount namespace (Linux host, root)
sudo nsenter --target "$(docker inspect -f '{{.State.Pid}}' goredis-diskfull)" --mount -- \
  mount -o remount,size=64m /data
sleep 2; redis-cli SET k v              # OK: the retry succeeded
redis-cli BGREWRITEAOF                  # compacts: -keyspace 100 means most records were overwrites
curl -s http://127.0.0.1:9379/metrics | grep -E '^goredis_aof_(healthy|size_bytes|rewrites_total)'

docker rm -f goredis-diskfull           # tmpfs content is discarded
```

A tmpfs disappears when the container stops, so you cannot run `goredis-check-aof` on it afterwards. For the full cycle including verification, use option B.

Option B, Docker with a small loop-mounted filesystem (Linux host):

```bash
truncate -s 8M /tmp/goredis-small.img
mkfs.ext4 -q /tmp/goredis-small.img
sudo mkdir -p /mnt/goredis-small
sudo mount -o loop /tmp/goredis-small.img /mnt/goredis-small
sudo chown 65532:65532 /mnt/goredis-small

docker run -d --name goredis-diskfull -e GOREDIS_PROTECTED_MODE=false \
  -v /mnt/goredis-small:/data -p 127.0.0.1:6379:6379 -p 127.0.0.1:9379:9379 goredis:dev
# ... fill it with goredis-bench as in option A, observe MISCONF ...

# Expand online
truncate -s 64M /tmp/goredis-small.img
LOOP=$(losetup -j /tmp/goredis-small.img | cut -d: -f1)
sudo losetup -c "$LOOP" && sudo resize2fs "$LOOP"
sleep 2; redis-cli SET k v && redis-cli BGREWRITEAOF

# Verify the file after a graceful stop
docker stop -t 30 goredis-diskfull
docker run --rm -v /mnt/goredis-small:/data --entrypoint goredis-check-aof goredis:dev /data/appendonly.aof

# Clean up
docker rm goredis-diskfull
sudo umount /mnt/goredis-small && rm /tmp/goredis-small.img
```

Option C, Kubernetes: in a test namespace, set the PVC request in `pvc.yaml` to the smallest size your StorageClass provisions (often 1Gi), fill it with the bench Job from Incident 1 using `-value-size 65536`, then follow the mitigation steps above, including the PVC expansion.

---

## Incident 3: A bad release raises latency or errors

### Symptoms

Right after a deploy, one or both of:

- Latency: clients time out or report slow writes. Reads may look fine.
- Errors: a specific client feature breaks, for example every `EXPIRE` returns `-ERR command 'EXPIRE' is disabled by server configuration`.

### Detection

- Alerts: `GoredisHighLatencyP99` (p99 > 10ms for 10m) and/or `GoredisHighErrorRate` (> 5% for 5m). `GoredisRecentRestart` fired a few minutes earlier, which points at the deploy.
- Dashboard: "Command latency p50 / p95 / p99" steps up at the deploy time; "AOF fsync latency p99" rises with it if the change concerns fsync. "Errors by command" shows which command fails.
- PromQL:

  ```promql
  sum by (command) (rate(goredis_command_errors_total[5m]))
  topk(5, histogram_quantile(0.99, sum by (command, le) (rate(goredis_command_duration_seconds_bucket[5m]))))
  ```

### Diagnosis

```bash
kubectl -n $NS rollout history deployment/goredis
kubectl -n $NS rollout history deployment/goredis --revision=5 -o yaml > rev5.yaml   # previous
kubectl -n $NS rollout history deployment/goredis --revision=6 -o yaml > rev6.yaml   # current
diff rev5.yaml rev6.yaml
# e.g.  - name: GOREDIS_AOF_FSYNC
#         value: always
#   or  - name: GOREDIS_DISABLED_COMMANDS
#         value: FLUSHDB,EXPIRE

# Live state vs. what is in git (also catches ConfigMap edits):
kubectl diff -k deploy/kubernetes

# The effective configuration as the container sees it:
kubectl -n $NS get configmap goredis-config -o yaml
kubectl -n $NS get deployment goredis -o jsonpath='{.spec.template.spec.containers[0].env}'
kubectl -n $NS logs $POD | head -n 20      # startup messages, including config errors
```

Correlate the deploy time (`kubectl -n $NS get rs -o wide`, ReplicaSet age) with the step in the latency or error panel.

### Mitigation

Roll back first, investigate second:

```bash
kubectl -n $NS rollout undo deployment/goredis
kubectl -n $NS rollout status deployment/goredis
```

`rollout undo` restores the previous pod template (image, env, resources). With `strategy: Recreate` this causes another short outage, which is acceptable compared with an ongoing error rate.

If the bad change was made in the **ConfigMap** rather than in the Deployment, `rollout undo` does not revert it (the ConfigMap is a separate object). Revert the ConfigMap from git and restart:

```bash
git checkout <last-good-commit> -- deploy/kubernetes/configmap.yaml
kubectl apply -k deploy/kubernetes
kubectl -n $NS rollout restart deployment/goredis
```

### Root cause

Examples of what this simulation reproduces:

- `GOREDIS_AOF_FSYNC=always` on network block storage. Every write waits for an fsync that takes several milliseconds on that storage, so write latency goes from microseconds to the fsync latency, and the p99 crosses 10ms.
- `GOREDIS_DISABLED_COMMANDS` extended with a command that clients use. Every call fails, and the error rate rises in proportion to that command's share of traffic.

In both cases the change passed review because it looked like a harmless config tweak, and nothing exercised it under realistic traffic before production.

### Prevention

- Deploy to a staging namespace first (a canary) with production-like storage and replayed or synthetic traffic (`goredis-bench`), and watch the p99 and error-rate panels before promoting.
- The CI smoke test catches a release that does not start, does not answer PING/SET/GET, or loses data across a restart. Extend `scripts/smoke.sh` when a regression slips through.
- Gate promotion on alerts: do not promote while `GoredisHighLatencyP99` or `GoredisHighErrorRate` fires in staging.
- Make ConfigMap changes visible to `rollout undo`: generate the ConfigMap with kustomize `configMapGenerator` (content-hashed name), so a config change creates a new ReplicaSet revision that can be undone.
- Pin image versions (tag or digest) so a revision always means the same binary.

### Verification

```bash
kubectl -n $NS rollout history deployment/goredis     # a new revision equal to the last good one
redis-cli -a "$PW" SET verify:k v EX 60 && redis-cli -a "$PW" EXPIRE verify:k 120
```

On the dashboard, p99 latency and the error rate return to the pre-deploy baseline within one 5-minute rate window, and the alerts resolve.

### How to simulate it safely

In a test namespace with some background traffic (the bench Job from Incident 1, with a smaller `-requests` and `-value-size 64`):

```bash
# Errors: disable a command the bench uses (it sends GET and SET)
kubectl -n $NS set env deployment/goredis GOREDIS_DISABLED_COMMANDS=FLUSHDB,GET
kubectl -n $NS annotate deployment/goredis kubernetes.io/change-cause="sim: disable GET" --overwrite
kubectl -n $NS rollout status deployment/goredis
# The bench reports errors; the dashboard shows errors for the GET command.

# Latency: force fsync on every write
kubectl -n $NS set env deployment/goredis GOREDIS_AOF_FSYNC=always
# On fast local disks the effect may be small. In Docker you can throttle the device:
#   docker run --device-write-bps /dev/sda:1mb -e GOREDIS_AOF_FSYNC=always ...

# Diagnose and roll back as described above
kubectl -n $NS rollout history deployment/goredis
kubectl -n $NS rollout undo deployment/goredis

# Finally restore exactly what is in git
kubectl apply -k deploy/kubernetes
```

`kubectl set env` writes the variable into the pod template, which overrides the ConfigMap value from `envFrom` and creates a new revision, so `rollout undo` reverts it.
