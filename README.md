# Healthchecks for Kubernetes

A Go controller that automatically monitors Kubernetes CronJobs in [Healthchecks](https://github.com/healthchecks/healthchecks), including self-hosted instances. It watches CronJobs and Jobs; no changes to job commands, container images, or application code are required.

This is an independent project and is not affiliated with or endorsed by Healthchecks.io. It uses Kubernetes client-go informers, a retry queue, and leader election.

## What it does

- Creates one Healthchecks check per cluster, namespace, and CronJob name.
- Syncs schedules, timezones, grace periods, names, tags, descriptions, and notification integrations.
- Reports Job start and final success/failure, with the Job UID as Healthchecks' run ID.
- Waits for terminal Job conditions: failed Pods and ongoing retries do not count as a failed Job.
- Pauses checks for suspended, excluded, or deleted CronJobs, preserving their history.
- Retries failed requests and periodically reconciles state after watch interruptions.
- Records accepted signals in Job annotations so controller restarts do not normally replay them.
- Uses a Kubernetes Lease to prevent duplicate delivery from replicas of the same installation.

Requires the Healthchecks Management API v3 and Kubernetes with `batch/v1` CronJobs (1.21+). `spec.timeZone` requires a Kubernetes version supporting that field (stable in 1.27). The controller uses a project **read-write API key**. The project's ping key is not needed: ping URLs come from the Management API.

## Build and test

Use Go 1.25 or later and Helm 3. Dependency download requires network access.

```sh
go mod download
make lint test build chart
```

The tests use a fake Kubernetes client and mocked HTTP transport. They do not contact a real cluster or Healthchecks project. `make test` runs the race detector. CI also builds the container image.

```sh
docker build --build-arg VERSION=dev -t healthchecks-kubernetes:dev .
docker run --rm healthchecks-kubernetes:dev --version
```

Licenses and notices of the Go runtime and all compiled-in dependencies are generated during the image build. `make licenses` writes the same file to `bin/THIRD_PARTY_LICENSES`.

## Container images

GitHub Actions builds and publishes `ghcr.io/felixtriller/healthchecks-kubernetes`
for `linux/amd64` and `linux/arm64` after the test job succeeds:

- Pushes to `main` publish `main` and `sha-<full-commit>` tags.
- Version tags such as `v0.1.0` publish `0.1.0`, `0.1`, and a commit tag.
  Stable version tags also publish `latest`; prereleases do not update `latest`.
- Pull requests and other branches run validation and a local image build without
  publishing. The workflow can also be started manually on `main` or a version tag.

Publishing uses the workflow's `GITHUB_TOKEN` with `packages: write`; no registry
password or Healthchecks credentials need to be configured in Actions. An image
is available only after its publishing run succeeds. Pin a release or a
commit-specific tag in GitOps. If the GHCR package is private, configure the
chart's `imagePullSecrets` with credentials that can read the package.

```sh
docker pull ghcr.io/felixtriller/healthchecks-kubernetes:0.1.0
```

## Install

Create a namespace and a Secret containing `HEALTHCHECKS_API_KEY`. A local `.env` file is ignored by Git and excluded from the Docker build context. Copy `.env.example` only if you do not already have `.env`, then fill in your instance URL and key.

```sh
kubectl create namespace monitoring
kubectl -n monitoring create secret generic healthchecks-api --from-env-file=.env
```

The chart defaults to release `0.1.0` with `image.pullPolicy=IfNotPresent`.
Both the chart and binary default to `https://healthchecks.io/api/v3`.
For a self-hosted instance, override `config.apiUrl` as shown below.
Install after the release image has been published:

```sh
helm upgrade --install healthchecks ./charts/healthchecks-kubernetes \
  --namespace monitoring \
  --set config.apiUrl=https://healthchecks.example.com/api/v3 \
  --set config.cluster=production-eu \
  --set credentials.existingSecret=healthchecks-api
```

Choose a stable, unique `config.cluster` for each cluster sharing a Healthchecks project. Changing it creates a new set of checks. A cluster name can contain lowercase letters, digits, and hyphens, with a maximum of 63 characters.

The chart runs one replica and uses a Recreate deployment strategy. Leader election also protects against overlap during shutdown and restart. Leadership can take up to approximately 30 seconds to transfer.

By default, all CronJobs are included. For a limited rollout, set `config.defaultInclude=false` and annotate the selected CronJobs with `healthchecks-kubernetes.felixtriller.github.io/include: "true"`.

For one namespace, set `config.namespace=jobs`. This renders a Role and RoleBinding in that namespace instead of cluster-wide permissions. The watched namespace must already exist. The ServiceAccount, Secret, controller state, and Lease stay in the Helm release namespace.

Do not install controllers with overlapping watch scopes for the same cluster/project. They would manage the same checks under different ownership tags. Keep the Helm release name and namespace stable as well: they identify the persistent state ConfigMap and leader Lease.

## CronJob annotations

Put these on the CronJob's `metadata.annotations`, not on its Job or Pod template.

| Annotation | Meaning | Default |
| --- | --- | --- |
| `healthchecks-kubernetes.felixtriller.github.io/include` | Opt a CronJob in; accepts a boolean string | Chart default |
| `healthchecks-kubernetes.felixtriller.github.io/exclude` | Opt a CronJob out; `true` takes precedence over include | `false` |
| `healthchecks-kubernetes.felixtriller.github.io/name` | Dashboard name, at most 100 characters | `cluster/namespace/name` |
| `healthchecks-kubernetes.felixtriller.github.io/grace-seconds` | Grace period, 60–31536000 seconds | `300` |
| `healthchecks-kubernetes.felixtriller.github.io/channels` | Comma-separated integration IDs/names; `*` selects all; empty selects none | `*` |
| `healthchecks-kubernetes.felixtriller.github.io/tags` | Comma- or whitespace-separated extra tags | None |
| `healthchecks-kubernetes.felixtriller.github.io/description` | Description or runbook link | Empty |

The controller maintains its own tags for ownership, cluster, and namespace. Tags beginning with `hck-owner-` are reserved. Slugs are stable hashes of cluster/namespace/name; deleting and recreating the same CronJob preserves the check's history.

Schedules come from `spec.schedule`; standard Kubernetes macros such as `@daily` are expanded to five-field cron expressions. Timezones come from `spec.timeZone`. If omitted, `config.timezone` defaults to UTC: **set it to match your kube-controller-manager timezone** if that differs.

Healthchecks uses its grace period for late runs and for execution time after a start ping. Choose it to cover the expected runtime and scheduling delays.

The Kubernetes configuration is the source of truth. Changes to managed fields in the Healthchecks dashboard are overwritten during reconciliation. A paused check for an included, unsuspended CronJob is resumed. Manage suspension from Kubernetes.

## Delivery and recovery

The controller sends a start signal when a Job has `status.startTime`, and success/failure only when the Job has a true `Complete`/`Failed` condition. This monitors the Job lifecycle, including scheduling and image-pull delays; it does not measure the exact container execution time. Multi-Pod, parallel, and retrying Jobs produce a single Job result.

A new or resumed Healthchecks check does not alert until it receives a ping. The controller therefore sends **one initialization success ping**, with a body explaining that monitoring was enabled and no Job execution is being reported. It uses the CronJob UID, separate from Job run IDs. This starts schedule monitoring even when the CronJob never launches. No synthetic start ping is sent for a Job already complete when discovered.

Accepted run signals and a hash of the check identity are saved in the `healthchecks-kubernetes.felixtriller.github.io/delivery` annotation on each Job. The secret check UUID and ping URL are not stored in Kubernetes annotations. This requires Job `patch` permission. The controller does not modify Job specs, CronJob specs, or Pod specs. If a ping succeeds but writing its annotation fails, retrying can repeat the ping. Delivery is **at least once**, not exactly once; a network timeout after server acceptance has the same ambiguity.

A controller-created ConfigMap stores the first activation time. Completions older than that are ignored on first installation; later retained completions can be recovered after restarts. Do not delete this ConfigMap while expecting recovery across a restart. A Helm uninstall intentionally leaves the controller-created ConfigMap in place.

Retain finished Jobs longer than the longest controller outage you want to recover from. Both `ttlSecondsAfterFinished` and CronJob history limits can delete them. Jobs removed before the controller reads their final state cannot be recovered. Very short Jobs may produce only a completion signal if they finish before their start is processed.

The controller processes retained completions oldest first. Healthchecks records reception times; events are not backdated. Reported durations include delivery and rate-limit delays, and recovered run durations may be inaccurate. Overlapping Jobs have distinct run IDs for duration matching, but Healthchecks still has one aggregate status per check.

Management requests are spaced at least 650 ms apart. Pings are spaced at least 13 seconds apart per check to stay within Healthchecks' documented five-pings-per-minute limit. A deferred ping does not block other CronJobs. High-concurrency schedules may build a backlog.

Every check is re-synchronized once per `config.resync` and a single worker delivers signals in order, so worst-case delivery latency grows with the number of monitored CronJobs, roughly 1.5 seconds per CronJob. With several hundred CronJobs, raise the grace period or the resync interval so success pings arrive within the grace period.

Suspended CronJobs do not emit run signals. Their checks are paused. On resumption, retained, unreported Job outcomes may be delivered. Exclusion and deletion pause checks at the next cleanup pass. Checks are never automatically deleted, and cleanup only touches checks with this controller's exact ownership tag.

The controller does not collect Pod logs, adopt existing manual checks, or instrument ordinary Jobs without a controlling CronJob owner reference.

## Isolated live smoke test

With access to a test cluster and the API credentials in `.env`:

```sh
python3 scripts/smoke-test.py --context '<test-context>'
```

This creates a temporary namespace, builds the controller inside a Go init container,
and tests discovery, successful and failed Jobs, schedule changes, suspension,
resumption, restart, and deletion. It needs no local Docker daemon or image registry.
The build container is limited to 2 CPUs and 2 GiB of memory and needs outbound
access to the Go module proxy. Allow up to ten minutes for the first build.

The test watches only its temporary namespace and disables notification channels.
It uses your real Healthchecks instance,
then deletes the namespace and only the checks carrying its exact test ownership
tag and name prefix. If interrupted with SIGKILL or if cleanup cannot reach either
API, remove the reported test namespace and its test-owned checks manually.

## Local execution

Use an explicit kubeconfig. Running this command creates/updates real checks and writes delivery annotations to the selected Jobs, so begin with opt-in monitoring:

```sh
set -a
. ./.env
set +a

go run ./cmd/healthchecks-kubernetes \
  --kubeconfig="$HOME/.kube/config" \
  --cluster=development \
  --state-namespace=monitoring \
  --default-include=false
```

The runtime does not automatically load `.env`. API keys are read from `HEALTHCHECKS_API_KEY`, not command-line arguments. `HEALTHCHECKS_API_URL` supplies the default for `--api-url`. All other configuration is available through `--help`.

The process serves `/healthz` and `/readyz` on port 8080. Readiness requires leadership and synchronized Kubernetes caches; it does not guarantee Healthchecks is reachable. Watch controller error logs for delivery failures. HTTP error messages omit request URLs and response bodies because these may contain credentials.

## License

Licensed under the [MIT License](LICENSE). Container images include this license
and the generated licenses and notices of the Go runtime and all compiled-in
dependencies in `/usr/share/licenses/healthchecks-kubernetes/`.

## References

- [Healthchecks Management API](https://healthchecks.io/docs/api/)
- [Healthchecks Pinging API](https://healthchecks.io/docs/http_api/)
- [Kubernetes Job lifecycle](https://kubernetes.io/docs/concepts/workloads/controllers/job/)
- [Cronitor Kubernetes agent](https://github.com/cronitorio/cronitor-kubernetes), the MIT-licensed project that inspired this controller.
