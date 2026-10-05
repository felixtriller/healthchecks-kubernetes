# Project guidance

This is an independent Go controller for monitoring Kubernetes CronJobs with
Healthchecks, including self-hosted instances. Read `README.md` for configuration,
deployment instructions, and delivery limitations.

## Code layout

- `cmd/healthchecks-kubernetes`: flags, environment variables, Kubernetes client,
  leader election, and health probes.
- `internal/controller`: CronJob configuration, watches, reconciliation, ownership,
  and persistent delivery checkpoints.
- `internal/healthchecks`: Management API v3 and Pinging API client.
- `charts/healthchecks-kubernetes`: Helm deployment and RBAC for both watch scopes.
- `scripts/smoke-test.py`: isolated live test using a temporary namespace.

Keep Kubernetes reconciliation separate from the HTTP client. Use the existing
client-go informers and retry queue. Keep flags, Helm values, and README guidance
consistent when changing configuration.

## Behavior to preserve

- Check identity depends on cluster, namespace, and CronJob name, not CronJob UID.
  Changing the identity or ownership hashes requires a migration plan.
- Use terminal Job conditions for success/failure. Failed Pods and retries are not
  final Job failures. Use the Job UID to correlate run signals.
- Only reconcile Jobs controlled by the current CronJob UID; ignore Jobs left
  over from an older CronJob with the same name.
- Record delivery only after Healthchecks accepts a ping. Store a hash of the
  check identity in Job annotations, never the secret UUID or ping URL.
- Delivery is at least once. Preserve restart recovery and the activation cutoff
  stored in the controller's ConfigMap; do not claim exactly-once delivery.
- Pause suspended, excluded, or deleted CronJobs' checks. Production cleanup must
  preserve history and only touch checks carrying the exact ownership tag.
- Healthchecks resume returns a check to `new`. Keep the labeled initialization
  ping so monitoring works even when a CronJob never launches.
- HTTP 200 alone does not prove a UUID ping was accepted: validate the response
  body. Preserve request timeouts, rate limits, retries, and redirect rejection.
- Keep reconciliation serialized unless per-check ordering and delivery safety
  are explicitly maintained. Run the controller only while holding leadership.
- Do not add Pod log collection, third-party telemetry, or modifications to
  CronJob, Job, or Pod specs as incidental changes.

## Validation

For Go changes, run `gofmt` on changed files and `make lint test build`.
`make test` includes the race detector. Use the existing fake Kubernetes client
and mocked HTTP transport for tests; ordinary tests must not contact live services.
Add regression tests for changed delivery or lifecycle behavior.

For Helm changes, run `make chart` and render the namespace-scoped variant:

```sh
helm template scoped charts/healthchecks-kubernetes --namespace monitoring \
  --set config.cluster=ci --set config.namespace=jobs \
  --set credentials.existingSecret=healthchecks
```

Check both cluster-wide and namespace-scoped RBAC. Avoid wildcard permissions and
Secret read permissions. For dependency changes, update `go.mod` and `go.sum`
together and check `go mod tidy -diff`. Documentation-only changes need a diff
review, not a live deployment.

## Credentials and live testing

- `.env` is local and ignored. Never print or commit its values, ping URLs, or
  real check UUIDs. Keep credentials out of image contexts, test source bundles,
  error messages, and command-line arguments. Use placeholders in examples.
- For live validation, use `python3 scripts/smoke-test.py --context k3s` with an
  explicit context. It creates real Kubernetes resources and Healthchecks checks.
- Preserve the smoke test's namespace isolation, opt-in discovery, disabled
  notification channels, and Cronitor exclusion annotations. Verify cleanup of
  both the temporary namespace and its test-owned checks.
- A successful smoke test does not authorize a permanent or cluster-wide rollout.
  Deployment configuration lives in `../gitops`; read that repository's
  `AGENTS.md` before changing it and follow its SOPS conventions for secrets.
