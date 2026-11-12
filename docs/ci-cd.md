# CI/CD

Two workflows live in `.github/workflows/`, plus `.github/dependabot.yml`.

| Workflow | Trigger | Permissions | What it does |
|----------|---------|-------------|--------------|
| `ci.yml` | push to `main`, every pull request | `contents: read` | Tests, image build + smoke test + vulnerability scan, manifest and monitoring config validation |
| `release.yml` | push of a tag matching `v*` | `contents: read`; the image job adds `packages: write` | Tests, then builds a multi-arch image and pushes it to GHCR |

`ci.yml` uses `concurrency` with `cancel-in-progress: true`, so a new push to the same branch or PR cancels the older run.

## `ci.yml` jobs

### test

| Step | Command | Catches |
|------|---------|---------|
| setup-go | `go-version-file: go.mod` (Go 1.27.4) | Toolchain drift between local and CI |
| gofmt | fails if `gofmt -l .` prints anything | Unformatted code |
| vet | `go vet ./...` | Suspicious constructs |
| test | `go test -count=1 ./...` | Regressions (`-count=1` disables the test cache) |
| race | `go test -race -count=1 ./...` | Data races in the server, store and AOF code |
| fuzz | `go test -run='^$' -fuzz=FuzzReadCommand -fuzztime=20s ./internal/protocol` | Parser panics and hangs on malformed input. 20s is a smoke-level run; run it longer locally. |
| build | `go build ./...` | Packages without tests that do not compile |
| govulncheck | `go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...` | Known vulnerabilities in dependencies and the standard library that the code actually calls |

### docker (needs: test)

1. `docker/setup-buildx-action`, then `docker/build-push-action` builds the image with `load: true` (into the local Docker daemon, not pushed), tag `goredis:ci`, and the GitHub Actions cache (`cache-from/cache-to: type=gha`).
2. `scripts/smoke.sh goredis:ci` starts the container and checks: `/readyz` becomes 200 within 30s, Docker health status becomes `healthy`, the user is `65532:65532`, `PING`/`SET`/`GET` over RESP, `/metrics` contains `goredis_commands_total`, `/healthz` is 200, a graceful `docker stop` does not end in SIGKILL, and `GET smoke:key` still returns `hello` after `docker start` on the same volume. Container logs are printed on failure.
3. Trivy (`aquasecurity/trivy-action`) scans `goredis:ci` for `CRITICAL,HIGH` vulnerabilities with `ignore-unfixed: true` and `exit-code: 1`: the job fails only on vulnerabilities that have a fix available.

### k8s-validate

1. Downloads kubeconform v0.8.0 and verifies its sha256 against the value from the release's `CHECKSUMS` file.
2. `kubeconform -strict -ignore-missing-schemas -skip ServiceMonitor deploy/kubernetes`: validates every manifest against the Kubernetes OpenAPI schemas. `-strict` rejects unknown fields (typos such as `readinessprobe`). `kustomization.yaml` has no schema and is skipped by `-ignore-missing-schemas`; the ServiceMonitor CRD is skipped explicitly.
3. `kubectl kustomize deploy/kubernetes | kubeconform -strict`: validates the rendered output that `kubectl apply -k` would send.
4. `promtool check config` and `promtool check rules`, run from the pinned `prom/prometheus:v3.16.0` image with `--entrypoint promtool`.
5. The Grafana dashboard is parsed with `python3 -m json.tool`.
6. `shellcheck scripts/*.sh` (shellcheck is preinstalled on GitHub's Ubuntu runners).

## Why actions are pinned to commit SHAs

A tag such as `v4` or `v4.2.2` is a mutable pointer. If an action's repository is compromised, the attacker can move the tag to malicious code, and every workflow using the tag runs it with that workflow's token and secrets. A full 40-character commit SHA cannot be moved. Each pin has a `# vX.Y.Z` comment recording which release it came from, and Dependabot (`github-actions` ecosystem) proposes updates that change both.

Pinned versions (each tag resolved through `GET /repos/<owner>/<repo>/git/ref/tags/<tag>`, annotated tags dereferenced to their commit, and cross-checked against `GET /repos/<owner>/<repo>/commits/<tag>`):

| Action | Version | Commit |
|--------|---------|--------|
| actions/checkout | v7.0.1 | `3d3c42e5aac5ba805825da76410c181273ba90b1` |
| actions/setup-go | v7.0.0 | `b7ad1dad31e06c5925ef5d2fc7ad053ef454303e` |
| docker/setup-buildx-action | v4.4.1 | `f87e5991a6d7451dcb8d9637bfbc97413f497069` |
| docker/build-push-action | v7.4.0 | `c3c9e263c25d99ce0380d002d59b67737d91b0dc` |
| docker/login-action | v4.8.0 | `8aa5831fb8d48d8f76594c7ddde61ae58019f550` |
| docker/metadata-action | v6.2.0 | `dc802804100637a589fabce1cb79ff13a1411302` |
| aquasecurity/trivy-action | v0.36.0 | `ed142fd0673e97e23eac54620cfb913e5ce36c25` |

Tools invoked by version are pinned too: govulncheck `v1.9.0` (Go module proxy), kubeconform `v0.8.0` (checksum verified), `prom/prometheus:v3.16.0`. `actions/checkout` runs with `persist-credentials: false` so the token is not left in `.git/config` for later steps.

## Reproducing CI locally

| CI step | Local command |
|---------|---------------|
| gofmt | `make fmt-check` (fix with `make fmt`) |
| vet | `make vet` |
| test | `make test` |
| race | `make race` (needs CGO and a C compiler: on Windows use WSL or install gcc) |
| fuzz | `make fuzz FUZZTIME=20s` |
| build | `go build ./...` or `make build` |
| govulncheck | `go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...` (also part of `make lint`) |
| image + smoke | `make smoke` (builds `goredis:dev` and runs `scripts/smoke.sh goredis:dev`) |
| trivy | `docker run --rm -v /var/run/docker.sock:/var/run/docker.sock aquasec/trivy image --severity CRITICAL,HIGH --ignore-unfixed --exit-code 1 goredis:dev` |
| kubeconform | `kubeconform -strict -ignore-missing-schemas -skip ServiceMonitor -summary deploy/kubernetes` |
| rendered kustomization | `kubectl kustomize deploy/kubernetes \| kubeconform -strict -summary -` |
| promtool | `docker run --rm -v "$PWD/monitoring/prometheus:/etc/prometheus:ro" --entrypoint promtool prom/prometheus:v3.16.0 check rules /etc/prometheus/alerts.yml` (and `check config /etc/prometheus/prometheus.yml`) |
| dashboard JSON | `python3 -m json.tool monitoring/grafana/dashboards/goredis.json > /dev/null` |
| shellcheck | `shellcheck scripts/*.sh` |

To run whole workflows locally, [`act`](https://github.com/nektos/act) works for the `test` and `k8s-validate` jobs; the `gha` cache backend in the `docker` job does not work under `act`.

## Release flow

1. Merge to `main` with CI green.
2. Tag and push:

   ```bash
   git tag -a v0.1.0 -m "v0.1.0"
   git push origin v0.1.0
   ```

3. `release.yml` runs `go vet` and `go test -race`, then builds `linux/amd64` and `linux/arm64` images (cross-compiled in the builder stage, no QEMU) and pushes them to `ghcr.io/<owner>/<repo>` (lowercased) using `GITHUB_TOKEN` with `packages: write`. Tags: `0.1.0`, `0.1`, and `sha-<full commit>`. The build sets `VERSION=v0.1.0` (shown by `goredis -version` and `goredis_build_info`) and attaches SBOM and provenance attestations.
4. The first push creates a private package. Make it public, or give the cluster an image pull secret, in the package settings on GitHub.
5. Deploy by updating the image tag or digest in `deploy/kubernetes/kustomization.yaml` and applying it (see [kubernetes.md](kubernetes.md#upgrading)). Get the digest with `docker buildx imagetools inspect ghcr.io/<owner>/<repo>:0.1.0`.

Tags are created by people with write access only; pull requests (including from forks) never trigger `release.yml` and never receive `packages: write`.

## Why there is no automatic deploy

There is no target cluster and no cluster credentials for this project, so a deploy job would have nothing to authenticate against. Adding one would mean storing a kubeconfig or cloud credentials as a repository secret, which is a liability with no benefit here. Deployment is a manual, reviewed `kubectl apply -k` (see [kubernetes.md](kubernetes.md)).

If a cluster is added later, the recommended shape is: a separate `deploy.yml` triggered by a published release or `workflow_dispatch`, a GitHub Environment with required reviewers, short-lived credentials through OIDC federation (no long-lived kubeconfig secret), deploy to a staging namespace first, wait for `rollout status` and the smoke checks, then promote. Alternatively a GitOps controller (Argo CD, Flux) pulls from the repository and no CI credentials are needed at all.

## Dependabot

`.github/dependabot.yml` checks weekly:

- `gomod` (`/`): Go module dependencies, grouped into one PR.
- `github-actions` (`/`): action SHAs and their version comments, grouped.
- `docker` (`/`): the `golang` builder tag and the distroless digest in the `Dockerfile`.
- `docker-compose` (`/deploy/docker`): the Prometheus and Grafana images.

Each Dependabot PR runs the full CI, including the smoke test and Trivy scan.

## Verify workflow (real infrastructure)

`.github/workflows/verify.yml` runs on pushes to `main` and on demand
(`gh workflow run verify`). It deploys nothing outside the job.

| Job | What it proves |
|-----|----------------|
| `kubernetes (kind)` | Builds the image, creates a kind cluster (kind binary checksum-pinned), creates the password Secret, and applies the **real** `deploy/kubernetes` manifests with only the image swapped. Then checks: rollout completes, the PVC binds, the pod is Ready with 0 restarts, `runAsUser=65532`, `readOnlyRootFilesystem=true`, JSON logs that never contain the password, `NOAUTH` without AUTH, `FLUSHDB` disabled, metrics and `/healthz`. Finally it runs `kubectl rollout restart` and checks that the old pod logged a graceful AOF flush and that the new pod replayed the data (`survived-restart`). |
| `linux benchmarks` | Runs `go test -bench` and `goredis-bench` under `everysec` and `always` on Linux, writes the results to the job summary, and uploads them as the `linux-bench` artifact. |

kind's default CNI does not enforce NetworkPolicy, so the NetworkPolicy is
schema-validated (`k8s-validate`) but not exercised here.

First verified run: `verify` run 43180662416 and `ci` run 43180662218
(commit `4e738cc`, 12 Nov 2026). All 5 jobs passed. Trivy reported 0
vulnerabilities in the image OS layer and all three binaries.
