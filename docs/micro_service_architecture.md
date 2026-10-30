# Microservice Architecture Proposal

**Status:** Target architecture remains a draft for review. Step 1 baseline work is recorded in [the baseline guide](baseline.md); service extraction and infrastructure changes have not started.

## Goal and Scope

Split the Nordik Drive monolith into services that can be developed, tested, deployed, scaled, and rolled back independently. Establish clear ownership of application data and preserve existing functionality throughout the transition.

The migration is incremental: the gateway sends extracted routes to their owning services and keeps remaining routes on the monolith. This proposal does not authorize application, infrastructure, or production changes before review.

## Confirmed Decisions

- Keep the extracted Go services in this repository, initially using the existing Go 1.25 module.
- Deploy each service independently on Cloud Run with its own image, configuration, service account, database role, and pipeline.
- Default every proposed Cloud Run service, including the gateway and its adapter, to request-based billing and zero minimum instances. The architecture must work without always-on application replicas.
- Use Google Secret Manager for application credentials, with per-secret IAM grants and version-pinned Cloud Run references. Do not deploy a separate secret-management server.
- Use short-lived workload credentials for NIA/service calls and automatic IAM database authentication for the existing Google Cloud SQL PostgreSQL instance. Stored runtime database passwords and shared NIA access keys are not the target authentication mechanism.
- Use separate PostgreSQL schemas and restricted credentials within one database, rather than separate database instances.
- Use the existing PostgreSQL database for initial shared rate-limit state, subject to a load and cost benchmark. A separate Redis deployment is not an initial requirement.
- Migrate without planned write downtime. Use compatible releases and bounded-lock schema changes instead of scheduled write pauses.
- Preserve existing frontend behavior, public API contracts, login cookies, and application user IDs during extraction.
- Extract Notifications first, after the gateway and shared foundations are ready.
- Require owner APIs for all cross-service data access in the final architecture, including NIA retrieval. Cross-domain database functions and replicated read models are not exceptions.
- Keep Python NIA in its existing repository and deployment. Plan companion NIA changes explicitly.
- Defer Keycloak user login until a later phase that includes coordinated frontend changes. Do not use Keycloak as an additional service-token issuer during this migration.
- Require at least 90% combined unit/integration statement coverage for each extracted service's handwritten runtime code, excluding generated bindings.

## Current Architecture and Problems

`cmd/server/main.go` starts one Gin application, connects feature modules to a shared GORM database, creates external clients, and starts an in-process scheduler. The current Dockerfile builds one server image, and the deployment workflow targets one Cloud Run service.

The package boundaries are useful starting points, but they are not independent service boundaries:

- File operations and admin searches join user tables. Extracting them without changing those queries would retain shared database ownership.
- File-version reconciliation updates form references, and file operations check pending submissions. These workflows currently depend on a shared transaction boundary.
- Chat reads file data and configuration directly. Its NIA adapter already uses gRPC and request cancellation, but does not attach a service identity token.
- NIA accesses record data through restricted database functions. That interface still crosses the proposed ownership boundary and must eventually become a Records API.
- Support scheduling owns a substantial workflow beyond generic notifications, including availability, assignments, approvals, and Zoom synchronization.
- Email delivery and scheduled jobs span several domains. Moving all jobs into Notifications would give it access to data that belongs elsewhere.
- Application activity records back `/api/logs`; they cannot be replaced solely by sending diagnostic output to Cloud Logging.

The baseline review identified login, password-verification, and password-reset requests being serialized into audit logs with credential fields. Step 1 replaces those payloads with explicit safe fields and adds storage-boundary redaction and regression tests. Historical exposure still requires the operator procedure in [the baseline guide](baseline.md#historical-credential-exposure-runbook); a source fix does not clean deployed logs.

## Target Architecture and Data Ownership

| Service | Responsibilities | Owned schema or persistence |
| --- | --- | --- |
| Identity | Users, existing authentication, OTPs, roles, and community memberships. Future Keycloak identity mapping belongs here. | `identity` |
| Records | Files, versions, normalization, access grants, forms, submissions, admin review, dataset configuration, lookup data, document content, and daily honours. | `records` |
| Support | Support requests, staff availability, assignments, calls, Zoom integration, and support maintenance. | `support` |
| Chat API | Public chat endpoints, audio processing, both existing chat modes, and the NIA adapter. Retrieve record data through Records APIs. | Stateless initially; NIA retains conversation persistence. |
| Notifications | Email delivery, provider configuration, delivery attempts, deduplication, and failed-delivery records. | `notifications` |
| Audit | Durable application activity ingestion and the existing `/api/logs` query behavior. | `audit` |

NIA remains an existing external service and owns its conversations, messages, and reasoning state. Its record retrieval must move to scoped Records APIs before final isolation is declared complete.

The gateway owns routing and edge controls, not business data. Its Go authorization adapter owns rate-limit counters in a dedicated `gateway` schema. Domain services cannot access that schema directly.

### Boundary Rationale

Keep the existing `admin` package with Records: its current endpoints search, review, and download file-related changes. The caller's administrator role does not establish a separate data domain.

Keep `file`, `formsubmission`, `dataconfig`, `lookup`, `logocontent`, and `honour` within Records initially. Preserve atomic file/form/version workflows instead of replacing their transactions with chains of remote calls. Move the honour text-generation adapter behind an interface owned by Records so it does not import Chat's database implementation.

Keep `supportrequest` and `supportschedule` together. Notifications delivers messages selected by those workflows; it does not own scheduling decisions or query support tables.

Share only platform utilities and versioned contracts. Do not share domain repositories, mutable ORM models, or concrete service implementations between services. Reuse existing interfaces where suitable and introduce interfaces at external boundaries rather than for every function.

### Ownership Rules

- A runtime database role can access only its service's schema and required platform objects. Migration credentials are separate from runtime credentials.
- Cross-domain references retain stable IDs. Replace cross-domain joins and database-level dependencies with owner APIs and explicit validation.
- Keep transactions within one service's owned data. Do not introduce distributed transactions for existing file/form operations.
- Use batched profile and membership APIs to replace user joins without creating a request per returned row. Preserve search, enrichment, pagination, and authorization behavior.
- Restrict Cloud Storage access by the service's needs; sharing a bucket does not grant unrestricted access to all objects.
- The remaining monolith may retain documented access during transition. Final ownership is not complete until that access and compatibility dependencies are removed.

## Gateway and Communication Contracts

### Gateway

Use Envoy Proxy as the initial gateway candidate on Cloud Run, with a local Go external-authorization adapter for cookie validation, rate-limit decisions, and audience-specific Google identity tokens. Validate the complete deployment's resource cost and latency before committing to the implementation. Configure explicit route-to-backend mappings, fail closed on authorization-adapter failures, and reject client-supplied workload tokens or identity headers as proof of trust.

Implement PostgreSQL-backed checks in that Go adapter through Envoy's external-authorization interface. This is custom adapter logic, not a database configuration change to Envoy's Redis-backed reference rate-limit service. Configure separate policies for authentication routes and business operations. Shared counters enforce each policy across gateway instances.

Preserve the existing public API hostname, public/authenticated route distinctions, cookies, CORS behavior, multipart uploads, downloads, streaming, content types, and filenames. Move the monolith to a private legacy backend before replacing the current public service with the gateway. Initially route all application traffic to that backend, then replace route groups incrementally.

Keep administrative listeners and internal worker endpoints outside the public route allowlist. Do not buffer entire upload bodies merely to perform authentication.

### PostgreSQL Rate Limiting

Start with fixed-window limits keyed by a stable policy ID, subject, and database-derived window start. Use a verified user ID for authenticated policies and a trusted client IP for anonymous policies. Define limits per route group rather than using arbitrary URL paths or caller-supplied identity headers as keys.

Use one atomic conditional `INSERT ... ON CONFLICT DO UPDATE ... RETURNING` operation to create or increment the counter only while capacity remains. Do not read a count and increment it in separate operations; concurrent Cloud Run instances could otherwise admit requests beyond the limit.

- Return HTTP 429 with the existing `error` field, a machine-readable code, and `Retry-After` when a window's quota is exhausted.
- Fixed windows allow a burst around the reset boundary. They are not an exact rolling-window quota; test and document this behavior for each policy.
- Use short, configurable acquisition, lock, and statement timeouts with a small dedicated connection pool. Budget total connections across the maximum gateway instance count so limiting cannot consume the business services' pool capacity.
- Fail closed with HTTP 503 when an authoritative check cannot complete. Do not report database unavailability as quota exhaustion or silently allow unlimited traffic. Exempt only explicitly allowlisted liveness/startup endpoints from the database check.
- Add a cheap, bounded local burst guard ahead of database access to reduce attack traffic and hot-key contention. Local counters are additional protection, not the shared source of quota decisions.
- Index window expiry and delete expired counters in bounded, scheduled batches. Bound key cardinality, local guard memory, and pending checks; monitor cleanup lag, row contention, query latency, and database write/pool pressure.
- Keep the limiter behind a small interface so storage can change if measured traffic outgrows PostgreSQL. Adding a managed Redis instance is a future capacity decision, not a prerequisite for extraction.

This approach avoids a separate cache deployment when the existing database has spare capacity. It adds an atomic database operation to each authoritative check and couples gateway availability to PostgreSQL. Benchmark realistic traffic, concurrent replicas, skewed/hot keys, bursts, and database failures before enabling it; include database sizing, connection usage, and any increased compute/I/O cost in the comparison.

### User and Service Authentication

Retain current user login and cookie contracts during extraction. Pin permitted JWT algorithms and validate signed user credentials in services; do not treat arbitrary forwarded user IDs as authenticated identity.

Use a dedicated Google service account per service, short-lived Google-signed ID tokens, and least-privilege Cloud Run invocation permissions. The token audience identifies the receiving service. Keep workload authentication separate from delegated end-user identity; where an application bearer token is forwarded, use the separate Cloud Run authorization header for workload credentials.

NIA clients must use this same workload-authentication mechanism instead of a shared NIA API key. Deploy token-capable callers before requiring authenticated invocation on NIA, verify the permitted caller accounts, and remove unauthenticated invocation access. Public browser clients continue through the gateway with application user credentials; never distribute service-account credentials or workload tokens to the browser. Any external machine client needs an explicit supported workload-federation/authentication path.

A valid calling service is not automatically authorized to perform every action for every user. The owning service checks resource permissions, memberships, and allowed system operations. Preserve NIA's existing principal grants and user/file/community isolation when changing its retrieval boundary.

Istio, a service mesh, and custom client-certificate management are outside the initial migration. Keycloak's later OIDC phase must preserve application user IDs and receive its own reviewed frontend, session, and account-migration plan.

### Short-Lived Credential Lifecycle

Secret Manager stores protected secret values and versions; it does not issue service identity tokens, dynamically create database credentials, or make a provider API key expire. The issuer or receiving provider must enforce credential expiry and revocation. Replacing a stored value or scheduling rotation alone does not invalidate a previously copied credential at its provider.

| Access boundary | Target mechanism | Lifetime and refresh |
| --- | --- | --- |
| Gateway/service calls, including NIA | Google-signed OIDC ID tokens from the attached Cloud Run service account, scoped to the receiving service audience. | Approximately one hour; reuse an audience-specific token source and automatically refresh before reuse after expiry. |
| Cloud SQL database logins, including the gateway limiter and NIA persistence | Automatic IAM database authentication through the Go or Python Cloud SQL language connector. | One-hour OAuth access tokens managed and refreshed by the connector; no static runtime database password. |
| Google APIs such as Storage, Vertex AI, and Secret Manager | Attached workload identity and Application Default Credentials. | Provider-managed short-lived access tokens, refreshed by supported libraries without service-account key files. |
| Other sensitive provider access | Provider-issued expiring credentials or a verified provider-supported key expiry/renewal mechanism. | Inventory each integration's real TTL, refresh authority, scopes, revocation behavior, and failure handling before implementation. |

Keep temporary tokens in process memory, not in Secret Manager versions, database tables, application logs, or images. Refresh on demand through supported libraries so all application services can scale to zero without a continuous token-refresh process. Reject expired, wrong-audience, or unauthorized caller credentials; do not fall back to a permanent shared key when refresh fails.

Enable Cloud SQL IAM database authentication and register each owning service account as an IAM database user. Grant the required Cloud SQL connection/login permissions and explicit PostgreSQL privileges only for its owned schema. IAM login permission does not itself grant access to application tables. Give migrations a distinct identity and privilege set. Use connector-managed encrypted connections and lazy/on-demand refresh suitable for request-based Cloud Run execution.

Token expiry prevents new database authentication with that token; it does not promise to terminate existing SQL sessions. Bound connection lifetimes and test new connections across refresh/expiry, reconnects after scale-to-zero, revoked identities, and the separately defined session-revocation procedure. Keep the existing password path only as a documented migration compatibility path until IAM-based clients and rollback have been verified, then remove runtime password access.

Classify signing/bootstrap secrets separately from runtime access tokens. Cover every sensitive credential, including SMTP, Zoom, JWT signing material, and third-party API keys, in a lifecycle inventory with an owner, consumers, expiry or rotation policy, renewal mechanism, and revocation test. A provider that cannot enforce the required short lifetime remains an unresolved design requirement; storing its static key in Secret Manager must not be described as meeting that requirement.

### Secret Management

Use Google Secret Manager for the remaining protected signing/bootstrap and provider credentials, including the legacy JWT signing secret and any required SMTP, Zoom, or third-party credential material. These stored values have their own lifecycle requirements; they are not automatically short-lived tokens. Keep non-sensitive settings such as service URLs, project IDs, and timeout values in ordinary configuration. User passwords remain hashed in Identity's database; Secret Manager does not replace password hashing or audit-log redaction.

Name secrets by environment, owning service, and purpose, for example `nordik-prod-notifications-smtp-password`. Separate development and production credentials. Each runtime service account receives `roles/secretmanager.secretAccessor` only on the secrets it requires; migration credentials have separate access. Preserve explicitly required sharing for legacy JWT verification until that authentication contract changes.

Use Cloud Run's managed secret-backed environment variables for versioned signing/bootstrap configuration during the initial migration, matching the existing environment-based configuration. Pin a numeric version in each service revision, resolve it at startup, and reject startup if required credentials are unavailable. Reference secret IDs and versions in deployment configuration without committing their values. Inject secrets only into containers that require them, including the gateway's Go adapter. Dynamic access tokens use refresh-capable clients/connectors instead of startup-only environment injection. Avoid per-request Secret Manager API reads and environment/configuration dumps in logs.

Continue using service identities and GitHub Workload Identity Federation for Google access. Do not create service-account JSON keys or store minted workload ID tokens in Secret Manager.

Rotate stored credentials at their issuing provider and roll out new Secret Manager versions through compatible service revisions with verified rollback. Keep overlapping credentials where supported. Any legacy database password retained during migration may require a replacement role with the same owned-schema grants while old revisions drain; the final runtime path uses IAM authentication. JWT signing-key rotation must preserve verification of previously issued user tokens for the agreed transition period. Retire old versions only after their consumers and rollback requirements are resolved. Disabled versions still incur storage charges until destroyed.

Plan for approximately 10-20 secrets. Secret Manager is a managed dependency that does not require an always-on application instance. With one active version per secret, automatic replication, and the available free allowance, illustrative storage cost is approximately CAD 0.35-1.20/month. This uses USD 0.06/version/month after six free versions and USD 1 = CAD 1.40; actual CAD SKU pricing, free-tier usage, version retention, and access counts must be checked. Retaining multiple enabled or disabled versions increases the billable version count. Access beyond the first 10,000 operations/month is USD 0.03 per 10,000 operations. Frequent cold starts contribute startup secret reads. Workload and database token issuance does not create a new Secret Manager version or require reading a stored service-account key; this storage estimate does not cover any additional provider-specific credential infrastructure.

### Synchronous Interfaces

Keep public HTTP/JSON routes and payloads compatible. Use versioned gRPC contracts for internal user lookups, batched profile retrieval, and scoped record queries; retain the existing NIA gRPC boundary.

Records APIs must support bounded, coarse-grained retrieval and query operations, rather than exposing arbitrary SQL or requiring one RPC per row. Adapt both the legacy Go chat implementation and NIA retrieval to these APIs. Keep conversation history in NIA and preserve the existing chat-backend configuration behavior; upstream failures must not silently switch backends.

Version internal contracts additively and test consumers against compatible old and new providers. Independent deployment must not require every service to upgrade simultaneously.

### Asynchronous Delivery

Use producer-owned transactional outboxes and Cloud Tasks for notification and audit delivery. Persist delivery intent with the owning business transaction. If enqueueing fails, keep the intent available for a bounded, scheduled dispatcher to recover it.

The owning domain selects recipients and notification content. Notifications receives a versioned delivery command with an idempotency key, content or template information, attachment references, expiry, and trace context. It must not read users, files, submissions, or support tables. Use object references instead of embedding large attachments in queue payloads.

Maintain persistent deduplication, bounded retries, delivery deadlines, and recoverable failed-delivery records. Expire time-sensitive messages consistently with their business validity. Treat uncertain SMTP outcomes explicitly: queue deduplication does not guarantee that an email is physically delivered exactly once.

Audit ingestion uses the same durable-delivery principles while retaining separate ownership and query behavior.

## Reliability and Observability Requirements

### Scale-to-Zero and Cost Controls

Set the initial minimum instance count to zero for every proposed Cloud Run service, including the gateway. Use request-based billing so idle instances without a configured minimum do not create reserved idle-compute charges. Scaling out and maximum instance counts must respect the shared database connection budget.

Cloud Tasks and Cloud Scheduler invoke services when work is available, allowing notification delivery, outbox dispatch, cleanup, and maintenance to resume from zero. Do not depend on continuous background goroutines, in-process cron, or an always-running dispatcher. Persist counters, outboxes, delivery/audit state, and job ownership in PostgreSQL; local caches and per-instance breaker state may reset after termination.

Test first requests through a fully cold gateway and backend chain, gRPC reconnection, startup delays, queued delivery, and cancellation. Include cold-start latency in operation budgets and retry only replay-safe work.

The current reported bill of approximately CAD 25-30/month is a cost reference, not evidence of future traffic or an approved spending forecast. With zero minimum instances, the earlier illustrative cost of keeping all services warm does not apply. Active request time, startup/shutdown, queued work, database load/storage, and observability can still increase the bill; long proxied NIA calls keep their request chain active. Cloud Run free allowances are shared across the billing account rather than granted anew to every service.

Measure incremental spending after each extraction against the existing workload. Keep the existing database and PostgreSQL limiter unless benchmarks justify additional infrastructure; do not introduce Redis, reserved warm instances, or always-on application compute as implicit migration dependencies.

### Contexts, Timeouts, and Concurrency

- Propagate `context.Context` through handlers, services, repositories, HTTP clients, gRPC clients, and database calls. Child deadlines must not exceed the remaining parent budget.
- Define configurable budgets for ordinary requests, chat, transfers, database connection acquisition, queries, and background work. Retain the current 300-second NIA budget as the starting baseline; lock operation-specific limits before each cutover.
- Bound connection pools, queued work, goroutines, and provider concurrency. Use durable work dispatch instead of unbounded request-spawned goroutines.
- Retry only transient failures that are safe to replay, using capped attempts and backoff within the caller's deadline. Do not blindly retry writes or NIA queries that persist messages without an idempotency contract.
- Preserve the existing `error` field and success payloads. Add machine-readable error codes, request IDs, and retry metadata without requiring frontend changes. Keep internal details out of public error messages.

### NIA Circuit Breaker

Use a configurable breaker per NIA upstream in each Chat API instance. Aggregate its telemetry across the fleet; the initial breaker is not a globally shared counter.

| State or event | Default behavior |
| --- | --- |
| Closed | Admit calls. Open after six final availability failures within a rolling 60-second window. |
| Open | Reject NIA calls for five minutes without invoking the upstream. |
| Half-open | Admit one trial request; close on success or reopen on failure. |
| Excluded failures | Do not count validation, authorization, domain conflicts, or caller cancellations. |

Count the final availability outcome after any permitted retry, not every attempt. Transient availability failures can contribute to the breaker when they remain unresolved; excluding all transient failures would prevent it from handling many outages.

Return HTTP 503 with `NIA not available at the moment. Please try again after some time.` and suitable retry metadata while open. Do not silently fall back to the legacy backend.

### Logs, Traces, Metrics, and Audit

Emit structured JSON diagnostic logs to stdout/stderr and collect them with Cloud Logging. Use managed retention and log-volume controls instead of application-managed rotating files in ephemeral Cloud Run containers.

Use OpenTelemetry and propagate trace/request identifiers through HTTP, gRPC, and queued work. Record service/revision, operation, authenticated caller service, trace/span IDs, status, duration, retry count, and relevant resource IDs. Do not log passwords, OTPs, authorization headers, cookies, or unrestricted request bodies.

Measure request duration, outbound calls, connection acquisition, connection establishment, and database query execution separately. Expose pool-wait and saturation metrics; cumulative database pool counters must not be presented as individual request wait times. Use resource metrics and profiling to investigate CPU/memory usage, because span duration alone does not measure those resources.

Support runtime log-level changes through an authenticated internal control and record those changes. Keep business audit records durable and separately queryable; apply credential redaction before either audit persistence or diagnostic export.

## Implementation Roadmap

Each phase requires a reviewed implementation specification and validation evidence before its production cutover.

1. **Establish the baseline.** Inventory routes, tables, joins, transaction boundaries, jobs, and external dependencies. Record every sensitive credential's issuer, owner, consumers, lifetime, renewal, and revocation mechanism. Align CI with Go 1.25, repair integration-test discovery, establish coverage collection, and address credential logging. Capture representative API behavior and performance. Track implementation and validation in [baseline.md](baseline.md).
2. **Introduce the gateway and foundations.** Benchmark the Envoy/Go adapter and PostgreSQL limiter, including their impact on business database workloads. Preserve the public API address, place the monolith behind the gateway, and initially forward all application routes to it. Establish workload tokens, automatic Cloud SQL IAM authentication, Secret Manager ownership/IAM and version-pinned bootstrap configuration, contexts, telemetry, rate limiting, contract checks, and independent pipelines. Coordinate authenticated NIA invocation with its callers. Move cron triggers out of replica-local schedulers before parallel deployments can duplicate jobs.
3. **Extract Notifications.** Introduce delivery commands, producer-owned outboxes, and durable dispatch. Move provider delivery and delivery state while keeping business decisions in their current domains. Verify notifications end to end before switching producers.
4. **Extract Identity, then Audit.** Preserve login and application IDs. Replace user joins with batch APIs and move audit ingestion/query ownership. Retain existing profile enrichment and audit filtering through owner APIs.
5. **Extract Support.** Move its complete workflow, tables, Zoom/provider integration, notification intents, and scheduled maintenance together.
6. **Extract Records.** Move connected file/form/review workflows together. Replace remaining external database dependencies. Coordinate NIA's scoped Records API integration in its existing repository before revoking retrieval access.
7. **Extract Chat and retire the monolith.** Preserve both chat modes and history behavior, remove remaining direct record access and cross-domain imports, revoke transitional grants, and verify independent deployments and rollbacks.

### Cutover and Rollback Rules

- Reuse existing physical tables where practical. Deploy compatible application releases before moving schemas or restricting ownership; do not assume compatibility views preserve every write/upsert behavior.
- Replace external callers with owner APIs before revoking access. Use bounded-lock, retryable schema changes and abort unsafe migrations rather than scheduling a write pause or blocking traffic indefinitely.
- Route new requests to the extracted owner and drain old handlers. Use one logical owning implementation and compatible revisions of its data contract; avoid application-level dual writes to separate stores.
- Give scheduled work a durable idempotency key or ownership lease. Move triggers to Cloud Scheduler or appropriate Cloud Run Jobs without allowing both old and new schedulers to perform the same operation.
- Retain a tested rollback path during each cutover. After ownership lockdown, roll back to a compatible prior service revision rather than restoring unrestricted monolith database access.
- Keep Go builds, images, configuration, migrations, and deployment workflows independently selectable. Changes to shared platform code or contracts must test affected consumers without requiring synchronized production deployments.

## Acceptance Criteria

- Existing application functionality and public API behavior remain compatible throughout extraction; Keycloak login changes are excluded from this migration.
- Each extracted service can deploy, scale, and roll back while other services remain on their current revisions.
- Every proposed Cloud Run service can scale to zero without losing durable work or depending on an always-on application process. Cold requests and scheduled/queued invocations recover within their validated operation budgets.
- Every application table, background job, external integration, and public route has an explicit owner.
- Runtime roles cannot read or write another service's schema. NIA uses scoped Records APIs, and no final cross-domain SQL exception remains.
- NIA and service calls use short-lived, audience-scoped workload tokens; database logins use automatically refreshed Cloud SQL IAM credentials. No permanent NIA access key or static database password remains in the final runtime configuration.
- Remaining signing/bootstrap/provider credentials are protected in Secret Manager and readable only by explicitly authorized consumers. Each sensitive credential has a reviewed lifetime and revocation mechanism; unsupported provider expiry is not silently treated as compliant. Cold-start loading, renewal, rotation, and rollback are tested without exposing values in source, images, deployment output, or application/audit logs.
- Successful business writes retain durable notification/audit intent, including when downstream services are unavailable.
- All requests and background work have bounded lifetimes and concurrency, with traceable failures and replay-safe retries.
- Rate limits hold across concurrent gateway instances within each configured fixed window. Counter cleanup and limiter load stay within the measured database budget, and limiter outages return the defined 503 response.
- Each extracted service meets the 90% combined unit/integration statement-coverage requirement for handwritten runtime code. The remaining monolith is not required to meet that threshold before the first extraction.
- No planned write downtime is introduced, and each cutover demonstrates compatibility and rollback under ongoing traffic.

## Verification and Baseline

Preserve existing test assertions and move tests with their owning implementation. Do not delete, skip, or weaken tests to make the migration pass. Correct legacy test discovery and strengthen coverage of actual failure modes.

| Area | Required scenarios |
| --- | --- |
| Compatibility | Login, refresh/logout, cookies, public/authenticated routing, permissions, uploads/downloads, API payloads, chat modes, and history. |
| Isolation | Denied cross-schema access, rejected unauthorized workloads, forged identity headers, and user/file/community scoping. |
| Secrets and tokens | Per-secret IAM denial, environment isolation, pinned-version startup, unavailable-secret failure, expired/wrong-audience NIA tokens, automatic refresh, IAM database reconnects, revoked access, stored-key rotation, rollback, and credential redaction. |
| Delivery | Redelivery, persistent deduplication, enqueue/provider failures, expired notifications, attachment retrieval, and failed-delivery recovery. |
| Reliability | Deadlines, cancellation, pool exhaustion, bounded concurrency, replay-safe retries, and all breaker transitions. |
| Rate limiting | Concurrent replica checks against one quota, window-boundary bursts, 429/retry metadata, spoofed identity/IP inputs, hot keys, expired-counter cleanup, bounded pools, and database failures returning 503. |
| Scale-to-zero and cost | A fully cold gateway/backend request chain, gRPC reconnection, queued/scheduled wake-up, durable outbox recovery, bounded cold-start concurrency, and measured incremental spending per extraction. |
| Observability | Cross-service/queue correlation, separate acquisition/query timing, runtime log-level controls, and credential redaction. |
| Migration | Compatible mixed revisions, uninterrupted writes, schema-lock contention, job ownership, independent deployment, and rollback. |

Use `go test ./... -count=1` for the default suite. Step 1 updates `make test-unit` to collect default coverage and `make test-integration` to run the full integration-tagged suite with a disposable PostgreSQL fixture and combined coverage. The three legacy integration suites now use discoverable `_integration_test.go` names and compatible packages. See [baseline.md](baseline.md#reproduce-validation) for setup and commands.

The earlier planning-time test attempts did not establish a usable baseline because of local toolchain/cache restrictions. Step 1 validation and coverage results supersede those attempts and are recorded in [baseline.md](baseline.md#verification-results), with explicit distinctions between synthetic local checks and production observations.

## Deferred Decisions and Review Gates

The architectural choices above define this roadmap. Before implementing each service, specify its exact route/RPC inventory, table migration, operation budgets, concurrency/rate limits, retry configuration, infrastructure sizing, and rollback checks using the measured baseline.

The database host is confirmed as Google Cloud SQL, and the NIA credential requirement concerns clients calling NIA. Automatic IAM database authentication and private, token-authenticated NIA invocation are therefore the selected mechanisms. Inventory any additional provider API keys separately and verify their actual expiry/refresh capabilities; a Secret Manager rotation schedule alone does not satisfy the short-lived-access requirement. Resolve provider limitations and budget any additional credential infrastructure before implementing the affected integration.

Keycloak hosting, account/password migration, OIDC browser flow, and session transition belong to the later coordinated identity feature. They must not be introduced indirectly through this backend extraction.

Implementation begins only after this architecture proposal has been reviewed. Companion NIA work is an explicit migration dependency, not permission to modify its repository during this documentation update.

## References

- [Cloud Run service-to-service authentication](https://docs.cloud.google.com/run/docs/authenticating/service-to-service): service accounts, token audiences, and separate workload authorization headers.
- [Authenticated gRPC calls to Cloud Run](https://docs.cloud.google.com/run/docs/samples/cloudrun-grpc-request-auth): reusable audience-specific ID token sources.
- [Envoy external authorization](https://www.envoyproxy.io/docs/envoy/v1.40.0/api-v3/extensions/filters/http/ext_authz/v3/ext_authz.proto): gateway authorization integration.
- [Envoy authorization responses](https://www.envoyproxy.io/docs/envoy/v1.40.0/api-v3/service/auth/v3/external_auth.proto): custom denial status, headers, and body for quota and limiter-failure responses.
- [PostgreSQL atomic upserts](https://www.postgresql.org/docs/15/sql-insert.html): concurrent `ON CONFLICT DO UPDATE`, conditional updates, and `RETURNING` behavior.
- [Cloud Tasks and Pub/Sub comparison](https://docs.cloud.google.com/tasks/docs/comp-pub-sub): explicit task delivery, bounded retries, and at-least-once semantics.
- [Cloud Run logging](https://docs.cloud.google.com/run/docs/logging): structured diagnostic output and managed collection.
- [Cloud Run pricing](https://cloud.google.com/run/pricing): active and minimum-instance idle billing, shared free allowances, and cost measurement.
- [Cloud Run Secret Manager integration](https://docs.cloud.google.com/run/docs/configuring/services/secrets): startup retrieval, version-pinned environment variables, and service identity permissions.
- [Cloud SQL IAM database authentication](https://docs.cloud.google.com/sql/docs/postgres/iam-authentication): short-lived database tokens and automatic refresh through language connectors.
- [Cloud SQL IAM login setup](https://docs.cloud.google.com/sql/docs/postgres/iam-logins): IAM database users, login permissions, and separate SQL grants.
- [Cloud SQL connections from Cloud Run](https://docs.cloud.google.com/sql/docs/postgres/connect-run): serverless connection and connector configuration.
- [Secret Manager best practices](https://docs.cloud.google.com/secret-manager/docs/best-practices): secret-level IAM and workload authentication.
- [Secret Manager pricing](https://cloud.google.com/secret-manager/pricing): active versions, replication locations, and access-operation charges.
- [Keycloak OIDC guidance](https://www.keycloak.org/securing-apps/oidc-layers): browser authorization flows and the guidance against direct password grants.
