# Milestone 2: gateway and foundations

This repository contains the implementation and a manual provisioning package.
No Google Cloud resources are provisioned by the application or its tests.
Service extraction starts with Notifications in Milestone 3.

The detailed nonproduction completion procedure and current shared-database
findings are in [the rollout runbook](milestone-2-rollout.md). Terraform now
defaults to metadata-only `bootstrap`; advance through `database`, `preview`
and `cutover` after verifying the corresponding prerequisites.

## Routing model

The gateway selects a service by a path boundary. The destination service owns
its endpoint router, request validation, resource permissions, and 404/405
responses. New endpoints within an existing boundary require no gateway change.

| Service key | Public boundaries |
| --- | --- |
| `identity` | `/api/user`, `/api/communities`, `/api/role` |
| `records` | `/api/file`, `/api/form`, `/api/admin`, `/api/config`, `/api/lookup`, `/api/logo-content` |
| `support` | `/api/support-requests`, `/api/support-schedule` |
| `chat` | `/api/chat` |
| `audit` | `/api/logs` |

Initially every group uses `LEGACY_BACKEND_URL` and
`LEGACY_BACKEND_AUDIENCE`. `SERVICE_BACKENDS_JSON` can override any group:

```json
{"records":{"url":"https://records.example.run.app","audience":"https://records.example.run.app"}}
```

Configure the receiving service's invoker IAM before changing its mapping.
Paths are preserved. The 95 public endpoints recorded in
`tests/fixtures/public_routes.json` are a regression inventory, not a runtime
endpoint allowlist. The baseline's 96th endpoint is the old internal honour
trigger. Worker and administrative paths have no public gateway mapping.

The Go sidecar implements Envoy v3 external authorization, verifies existing
cookies for protected boundaries, selects rate policies, and replaces incoming
workload/identity headers. Identity's mixed public/session endpoints and public
lookup/community endpoints retain their existing behavior. Domain services
continue checking signed user cookies and resource permissions themselves.

## Runtime behavior

- `cmd/server`: `APP_ROLE=monolith` runs the public application router;
  `APP_ROLE=worker` exposes only health, private scheduled-work handlers and
  configured operator controls. `cmd/gateway` runs the local authorization
  sidecar and renders Envoy configuration without embedding credentials.
- `SCHEDULER_MODE=legacy` retains guarded local cron during transition;
  `external` disables local cron, startup maintenance and the shared-secret
  honour trigger. `disabled` also disables those paths for local checks.
  Apply the additive platform migration before enabling guarded cron.
- External worker invocations require a dedicated **IAM-protected Cloud Run
  service**, with only the Scheduler account and designated operators admitted.
  Scheduler job-name/time headers identify delivery slots; they are not proof
  of authentication. The worker validates those identifiers after platform IAM.
- A persistent ledger records attempts and successful `(job, scheduled time)`
  slots. A session advisory lock serializes different slots of the same job.
  Failed/crashed slots can retry. Ownership loss cancels the running operation.
  Database release discards a connection if advisory unlock fails.
- Scheduled frequencies remain five minutes for the two email jobs, ten minutes
  for normalization, one minute for reconciliation, and Toronto midnight for
  honours/support maintenance. Existing domain locking/progress checks remain.
- SMTP attempts are bounded and finish before their request/job returns.
  Signup, password-change and support advisory email failures do not undo
  committed business changes. SMTP send-before-mark uncertainty remains;
  the transactional notification outbox/delivery ledger belongs to Milestone 3.
- Services receive request-local GORM copies. Every production ORM operation
  has a two-second connection-acquisition budget; database transactions retain
  their connection through commit/rollback. Query streams release their leases
  on close/EOF/cancellation. Driver statement caching remains enabled.
- Default budgets are 30 seconds for ordinary requests, 300 for chat/transfers,
  and 900 for jobs. Configure `REQUEST_TIMEOUT_SECONDS`, `CHAT_TIMEOUT_SECONDS`,
  `TRANSFER_TIMEOUT_SECONDS`, and `JOB_TIMEOUT_SECONDS` within 1–900 seconds;
  update infrastructure timeouts together if changing defaults. Parent
  cancellation/deadlines continue to take precedence.
- JSON diagnostics and OpenTelemetry trace context cover HTTP, NIA gRPC, SQL
  acquisition, connection establishment and query execution. Logs omit bodies,
  SQL parameters, cookie/token values and raw database connection errors.
  Pool wait totals remain aggregate metrics, not invented per-request timings.
  `OTEL_EXPORTER=gcp` enables Cloud Trace export; local tests use `none`.

`CORS_ORIGINS` optionally replaces the existing origin list. Request-ID and
retry headers are exposed for browser diagnostics. Operator controls are absent
unless `OPERATOR_TOKEN_AUDIENCE` and `OPERATOR_SERVICE_ACCOUNTS` are configured.
They require an original signed token in `X-Operator-Authorization`, separate
from Cloud Run's platform header. `POST /internal/log-level` accepts
`{"level":"DEBUG"}`; `GET /internal/metrics` reports aggregate pool statistics.
Mint operator tokens with a verified email claim (`--include-email`) and the
configured operator audience; use a separate token for Cloud Run invocation.
Log-level changes affect the receiving instance and are recorded; they do not
claim to persist across scale-to-zero or coordinate every replica.

## Rate limits and credentials

`RATE_LIMIT_MODE=observe` is the default. It records would-be 429/503 outcomes
without rejecting requests for shared-quota decisions. Cookie/workload failures
still fail closed. `enforce` returns 429 with `Retry-After` when exhausted and
503 when authoritative state is unavailable. A bounded local burst guard and
pending-check limit protect the database and adapter. Connection acquisition
defaults to two seconds (`RATE_DB_ACQUIRE_MS=2000`); the separate query budget
is 250ms (`RATE_DB_QUERY_MS=250`). Their sum must remain below 4500ms.

| Policy | Initial requests/minute | Configuration |
| --- | --- | --- |
| Identity authentication group, by client IP | 20 | `RATE_AUTH_PER_MINUTE` |
| Public operations, by client IP | 120 | `RATE_PUBLIC_PER_MINUTE` |
| Authenticated operations, by verified user | 120 | `RATE_USER_PER_MINUTE` |
| Chat group, by verified user | 10 | `RATE_CHAT_PER_MINUTE` |

These are calibration defaults, not measured production quotas. PostgreSQL's
clock defines fixed one-minute windows; boundary bursts are expected. One
conditional upsert enforces each shared quota. Expired counters are removed in
bounded batches by a finite job under the gateway identity.

Production requires `CLIENT_IP_MODE=forwarded` and a verified
`TRUSTED_PROXY_HOPS` suffix count. Do not trust the caller's first forwarded
address. Local fixtures use `GATEWAY_LOCAL=true` and `CLIENT_IP_MODE=peer`.
Local mode also permits HTTP upstreams and omits workload tokens; never enable
it on a deployed public service.

Production database configuration uses `DB_AUTH_MODE=iam`,
`CLOUD_SQL_INSTANCE`, `CLOUD_SQL_IP_TYPE`, `DB_USER`, and `DB_NAME`. The official
Cloud SQL connector provides encrypted transport, automatic IAM login and lazy
refresh. No password fallback occurs. `password` mode exists for explicit
local fixtures/transition. The gateway uses only its `gateway` schema; monolith
and worker retain documented transitional public-schema access. Runtime
identities receive no DDL grants. Apply schema changes with the migration/owner
identity before switching runtime accounts.

NIA uses `NIA_AUTH_MODE=iam`, TLS, `NIA_GRPC_TARGET=host:443`, and
`NIA_TOKEN_AUDIENCE=https://receiving-service.run.app`. Audience-specific token
sources refresh in memory; tokens are not stored in SQL or Secret Manager.
Unauthenticated NIA mode is limited to loopback fixtures. Token-capable callers
must be verified before removing NIA's existing public invocation grant.
NIA queries are not automatically replayed. The per-upstream circuit breaker
opens after six final availability failures within 60 seconds, waits five
minutes, then permits one probe. Validation/permission/domain errors and caller
cancellations do not contribute availability failures.

## Manual provisioning and rollout

Use the [development deployment guide](milestone-2-rollout.md) as the single
operational sequence. It explains where to run each command, why it is needed,
its effects and completion checks. The current development profile uses the
existing project and billing configuration, local Terraform state, the existing
public gateway service, one private backend and one worker. Separate private
preview infrastructure and manual GitHub deployment settings are optional and
are not prerequisites for this rollout.

Terraform stages used by that profile are bootstrap (metadata/IAM), database
(IAM database users), and cutover (runtime services and tagged public candidate).
The public candidate initially receives 0% traffic. Numeric Secret Manager
references are used; secret payloads never enter Terraform. Retain state and
review each saved plan before applying it. Existing resources are imported,
not duplicated.

The allocation guard remains 80% of verified SQL capacity. Controlled runtime
pools, replacements, NIA, cleanup, operator access and the user-provided outside
allocation are recorded separately. A reviewed allocation is not an enforced
quota on unchanged applications. Current activity snapshots are not maxima.
The development profile totals 17 of 20 allocated slots with one public gateway.

## Verification

On Windows:

```powershell
./scripts/test-milestone2.ps1 -WithEnvoy
```

On Linux, use the existing `make test-unit` and `make test-integration` targets
with the isolated `docker-compose.test.yml` fixture, then:

```sh
go test -tags=integration,envoyintegration ./tests/integration -run '^TestEnvoyServiceRoutingAndStreaming$' -count=1
go test -race ./internal/gateway ./internal/circuit ./internal/jobs ./internal/database ./internal/requestscope ./internal/mailer
terraform -chdir=infra/terraform init -backend=false
terraform -chdir=infra/terraform validate
terraform -chdir=infra/terraform test -filter=tests/safety.tftest.hcl
terraform -chdir=infra/terraform test -filter=tests/small_database.tftest.hcl -var-file=tests/fixtures/small-db.tfvars.example
terraform -chdir=infra/terraform test -filter=tests/rollout.tftest.hcl
```

The Terraform tests use a mock Google provider and never provision resources.
PostgreSQL tests require the named loopback-only synthetic fixture and create
random schemas that are removed afterward. The Envoy check starts separate
synthetic domain backends and verifies service dispatch, endpoint delegation,
cookies, application authorization headers, forged-header removal, CORS,
upload streaming, download filenames and authorization-sidecar outages.

Real Cloud Run IAM, Secret Manager versions, Cloud SQL renewal/revocation,
deployed NIA access, cold starts and actual spending must be verified after your
manual provisioning. Passing local fixtures is not evidence those cloud checks
have occurred.

### Recorded local results

Rechecked on 2026-11-06: application regression/integration, static and real
Envoy checks pass; combined coverage remains 70.4%, gateway library 98.6% and
worker core 96.1%. Linux race checks pass. The integrated Terraform module
validates and passes 38 mock checks (8 original, 14 budget, 16 rollout).
Workflow lint passes. NIA and unchanged outside consumers share the 25-slot
instance; outside application quotas are not changed by this release. Outside applications are
excluded from this release. A review-only bootstrap
plan has 44 additions, zero updates/deletes and has not been applied. None of
these results establishes cloud acceptance or milestone completion.

Verified on 2026-11-03; these are local/synthetic checks, not cloud acceptance.

| Check | Result |
| --- | --- |
| Default regression suite | 515 top-level tests pass; 866 pass results including subtests. |
| Integration-tagged regression suite | 559 top-level tests pass; 912 pass results including subtests. |
| Optional companion checks | Two existing Python NIA tests skip because their separate fixture is not configured. |
| Combined statement coverage | 70.4% across the repository; the remaining monolith has no 90% prerequisite. |
| New gateway library | 98.6% statement coverage (215/218). |
| New worker core (`runner`, `http`, `registry`) | 96.1% statement coverage (98/102). |
| Runtime concurrency | Linux race checks pass for gateway, circuit, jobs, database, request scope, mailer, workload, observability and chat packages. |
| Real proxy | Pinned Envoy 1.38.6 passes the separate-backend routing/streaming/failure test in an isolated Docker network. |
| Provisioning package | Terraform validates; all eight mocked-provider tests pass without contacting Google Cloud. |
| Builds/static checks | Server and adapter build for Windows and Linux; `go vet` and workflow lint pass. |

The coverage gate merges duplicate observations from combined profiles and
requires 90% for the new gateway library and worker core. It does not label
process startup, reused domain implementations or external Google APIs as
covered by those focused numbers. Existing assertions are retained; modified
legacy provider hooks only gained a context argument.

The build workflow avoids creating a Google credential file, and Docker/Git
ignore any `gha-creds-*.json` produced by other workflows. Credential files,
local environment files and Terraform state are excluded from image contexts.

Local database smoke benchmarks (Go 1.25.5, Windows/amd64, PostgreSQL 15,
Intel Core i7-1365U, fixed synthetic fixtures, three 1-second samples):

| Operation | Three samples, ms/op |
| --- | --- |
| Shared limiter check | 1.54, 1.61, 2.21 |
| File listing | 1.13, 1.72, 1.21 |
| Audit search | 6.62, 6.66, 6.67 |

These are individual service/database timings on a local fixture. They do not
measure a complete HTTP gateway chain, cloud cold starts, realistic traffic,
or interference under concurrent production workloads. Those rollout checks
remain required after provisioning. All owned fixture schemas and advisory
locks were gone before removing the temporary database container/network.
