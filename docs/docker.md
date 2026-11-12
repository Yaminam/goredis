# Running goredis with Docker

## Image layout

The `Dockerfile` at the repository root has two stages:

| Stage | Base | Purpose |
|-------|------|---------|
| `build` | `golang:1.27.4-alpine3.23` | Builds `goredis`, `goredis-check-aof` and `goredis-bench` with `CGO_ENABLED=0 -trimpath -ldflags="-s -w -X main.version=$VERSION"`. Uses BuildKit cache mounts for the module and build caches. Runs on the build host's platform and cross-compiles for the target platform. |
| runtime | `gcr.io/distroless/static-debian12:nonroot`, pinned by digest | No shell, no package manager. Contains the three binaries in `/usr/local/bin`, CA certificates and timezone data. |

Runtime defaults baked into the image:

| Setting | Value |
|---------|-------|
| User | `65532:65532` (distroless `nonroot`) |
| `GOREDIS_ADDR` | `0.0.0.0:6379` |
| `GOREDIS_ADMIN_ADDR` | `0.0.0.0:9379` |
| `GOREDIS_AOF_PATH` | `/data/appendonly.aof` |
| `GOREDIS_LOG_FORMAT` | `json` |
| Volume | `/data`, owned by 65532 |
| Ports | 6379 (RESP), 9379 (admin HTTP: `/metrics`, `/healthz`, `/readyz`) |
| Stop signal | `SIGTERM` |

## Build

```bash
docker build --build-arg VERSION=v0.1.0 -t goredis:dev .
# or
make docker-build IMAGE=goredis:dev
```

`VERSION` ends up in `goredis -version` and in the `goredis_build_info` metric. Optional build args `REVISION` and `CREATED` fill the OCI labels.

Multi-arch build (what the release workflow does):

```bash
docker buildx build --platform linux/amd64,linux/arm64 -t goredis:dev .
```

## Run

Protected mode is on by default. The container listens on `0.0.0.0`, so without a password the server refuses to start. You have two options.

Local development, ports bound to the loopback interface of the host only:

```bash
docker run -d --name goredis \
  -e GOREDIS_PROTECTED_MODE=false \
  -p 127.0.0.1:6379:6379 -p 127.0.0.1:9379:9379 \
  -v goredis-data:/data \
  goredis:dev
```

With a password (any other case). Pass the variable name only, so the value does not appear in your shell history or in `docker inspect` output of the command line:

```bash
export GOREDIS_REQUIREPASS="$(openssl rand -base64 32)"
docker run -d --name goredis \
  -e GOREDIS_REQUIREPASS \
  -p 127.0.0.1:6379:6379 -p 127.0.0.1:9379:9379 \
  -v goredis-data:/data \
  goredis:dev
redis-cli -a "$GOREDIS_REQUIREPASS" PING
```

Note that environment variables are still visible to anyone who can run `docker inspect` on the container. For a stronger setup, mount a file and set `GOREDIS_REQUIREPASS_FILE`:

```bash
docker run -d --name goredis \
  -e GOREDIS_REQUIREPASS_FILE=/run/secrets/goredis-password \
  -v "$PWD/goredis-password:/run/secrets/goredis-password:ro" \
  -p 127.0.0.1:6379:6379 -p 127.0.0.1:9379:9379 \
  -v goredis-data:/data \
  goredis:dev
```

The file must be readable by uid 65532.

Any server flag can be set through its environment variable (see `goredis -h`), for example `-e GOREDIS_MAXMEMORY=200mb -e GOREDIS_AOF_FSYNC=always`.

Do not publish 6379 on `0.0.0.0` of a host that is reachable from other machines without a password. The admin port has no authentication; keep it on 127.0.0.1 or a private network.

## Volumes and permissions

- The AOF lives in `/data`. Always mount a volume there, or the data is lost when the container is removed.
- A **named volume** (`-v goredis-data:/data`) is initialised from the image on first use and inherits the `65532:65532` ownership of `/data`. This is the simplest option.
- A **bind mount** (`-v /srv/goredis:/data`) keeps the host directory's ownership. Make it writable by uid 65532 first, or the server fails at startup with `permission denied`:

  ```bash
  sudo mkdir -p /srv/goredis && sudo chown 65532:65532 /srv/goredis
  ```

- The server takes an OS lock on `/data/appendonly.aof.lock`. Two containers pointed at the same volume cannot both run; the second one exits with an error that the lock is held.
- `--read-only` is supported as long as `/data` is a volume. Add `--tmpfs /tmp` for scratch space, as the Kubernetes manifests do with an `emptyDir`.

## Health check

The image declares:

```dockerfile
HEALTHCHECK --interval=10s --timeout=3s --start-period=60s --start-interval=2s --retries=3 \
    CMD ["/usr/local/bin/goredis", "-healthcheck"]
```

The distroless image has no shell or curl, so the binary probes itself: `goredis -healthcheck` sends `GET http://<admin-addr>/readyz` (with `0.0.0.0` replaced by `127.0.0.1`) and exits 0 on HTTP 200, 1 otherwise. The status is `starting` while the AOF loads, `healthy` once the listener accepts connections, and `unhealthy` after 3 failed probes. `--start-interval` needs Docker Engine 25 or newer; older engines ignore it and probe every 10s.

```bash
docker inspect --format '{{.State.Health.Status}}' goredis
docker inspect --format '{{json .State.Health}}' goredis
```

If you disable the admin server (`GOREDIS_ADMIN_ADDR=`), the health check fails. Override it with `--no-healthcheck` in that case.

## Stopping

`docker stop` sends SIGTERM. The server stops accepting connections, flips `/readyz` to 503, finishes in-flight commands, flushes and fsyncs the AOF and exits within `GOREDIS_SHUTDOWN_TIMEOUT` (default 20s). Docker's default stop timeout is 10s, so use a longer one to avoid a SIGKILL:

```bash
docker stop -t 30 goredis
```

The compose file sets `stop_grace_period: 30s` for the same reason.

## Tools inside the image

`goredis-check-aof` and `goredis-bench` are in the image. Since there is no shell, run them as the entrypoint of a one-off container:

```bash
# Check the AOF of a stopped server (do not run -fix while the server is running)
docker stop -t 30 goredis
docker run --rm -v goredis-data:/data --entrypoint goredis-check-aof goredis:dev /data/appendonly.aof

# Load test against the running server
docker run --rm --network host --entrypoint goredis-bench goredis:dev \
  -addr 127.0.0.1:6379 -clients 50 -requests 100000 -pipeline 16
```

`--network host` works on Linux. On Docker Desktop use `-addr host.docker.internal:6379`.

## Compose stack: goredis, Prometheus, Grafana

`deploy/docker/docker-compose.yml` builds the image from the repository root and starts:

| Service | URL | Notes |
|---------|-----|-------|
| goredis | `127.0.0.1:6379`, http://127.0.0.1:9379/metrics | Named volume `goredis-data`. |
| Prometheus | http://127.0.0.1:9090 | Loads `monitoring/prometheus/prometheus.yml` and `alerts.yml`. 7 days retention. |
| Grafana | http://127.0.0.1:3000 | Datasource and the "goredis" dashboard are provisioned from `monitoring/grafana/`. Anonymous users get Viewer access. Admin login: `admin` / `admin` unless `GRAFANA_ADMIN_PASSWORD` is set. |

```bash
make compose-up        # docker compose -f deploy/docker/docker-compose.yml up -d --build
make compose-down      # stops containers, keeps volumes
docker compose -f deploy/docker/docker-compose.yml down -v   # also deletes the data volumes
```

All ports are bound to 127.0.0.1. goredis runs with `GOREDIS_PROTECTED_MODE=false` because of that. If you change the port bindings to another interface, set a password first.

Image versions are pinned: `prom/prometheus:v3.16.0`, `grafana/grafana:13.3.1`. Dependabot proposes updates.

### Setting a password with an untracked `.env`

Compose reads `deploy/docker/.env` (the directory of the compose file) automatically and substitutes `${...}` variables. `.env` is listed in `.gitignore`; never commit it.

```bash
cat > deploy/docker/.env <<EOF
GOREDIS_REQUIREPASS=$(openssl rand -base64 32)
GOREDIS_PROTECTED_MODE=true
GRAFANA_ADMIN_PASSWORD=$(openssl rand -base64 24)
EOF
chmod 600 deploy/docker/.env
make compose-up
```

Check that the values are picked up without printing them in logs you share: `docker compose -f deploy/docker/docker-compose.yml config` renders the file with the values substituted, so treat its output as secret.

## Smoke test

`scripts/smoke.sh [image]` runs the same checks as CI against a local image: readiness, health status, PING/SET/GET over `/dev/tcp`, `/metrics`, graceful stop and restart with the data intact, and the runtime user.

```bash
make smoke                                 # builds goredis:dev and tests it
SMOKE_RESP_PORT=16379 SMOKE_ADMIN_PORT=19379 scripts/smoke.sh goredis:dev   # if 6379/9379 are busy
```
