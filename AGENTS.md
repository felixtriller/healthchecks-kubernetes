# Project guidance

Read `README.md` for configuration, architecture, and delivery limitations.

## Non-obvious invariants

- Check identity uses cluster/namespace/CronJob name, not UID. Changes to identity,
  ownership hashes, or annotation keys need a migration plan.
- Only process Jobs owned by the current CronJob UID. Report final outcomes from
  terminal Job conditions, not failed Pods or retry counts; correlate with Job UID.
- Record delivery only after an accepted ping, validating both HTTP status and
  response body. Store a hash of check identity, never its secret UUID or ping URL.
- Preserve at-least-once recovery, the persisted activation cutoff, per-check ping
  ordering, and rate limits. Reconcile only while holding leadership.
- Pause suspended, excluded, or deleted CronJobs' checks; preserve history and only
  clean up checks carrying the exact ownership tag.
- Resuming a Healthchecks check returns it to `new`; keep the labeled initialization
  ping so monitoring starts even if the CronJob never runs.

## Validation and boundaries

- Use fake Kubernetes clients and mocked HTTP for ordinary tests. Add regression
  coverage for delivery/lifecycle changes; follow the Makefile and CI for checks.
- Helm changes must render both cluster-wide and namespace-scoped RBAC. Avoid
  wildcard permissions and Secret read access.
- Keep secrets out of logs, examples, image contexts, and test bundles.
- Live smoke tests must retain namespace isolation, opt-in discovery, disabled
  notifications, and cleanup of their Kubernetes resources and owned checks.
- PR builds must not log in to registries or publish images. Scope package write
  access to publishing jobs; preserve amd64/arm64 cross-compilation.
