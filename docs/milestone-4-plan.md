# Milestone 4: Extract Identity, then Audit

This records the previously approved Milestone 4 plan. Milestone 3 development acceptance was completed on 12 November 2026; its prerequisite is satisfied. Milestone 4 implementation is in progress on `feature/milestone-4-identity-audit`; cloud rollout and acceptance remain gated.

The implementation's staged commands, migration gates and compatible rollback procedure are in the [development rollout guide](milestone-4-rollout.md).

## Brief outline

1. Establish deployment coordination within the existing database capacity.
2. Extract Identity and replace external user-table queries with owner APIs.
3. Extract Audit, preserve activity search, and complete compatibility checks.

Development only. Preserve existing cookies, JWT values, user IDs, public URLs, signup UI, and database tier.

## Identity extraction

Create private `nordikdriveapi-identity` with its own image, runtime identity, and restricted database role. Start with minimum zero, maximum one instance, and one database connection.

Identity owns users, existing authentication, roles, communities, password-reset OTPs, signup verification, and its notification/audit outboxes.

Move these existing public route groups behind Identity:

| Route group | Behavior |
| --- | --- |
| `/api/user` | Preserve login, signup, OTP, password reset, refresh, logout, and user responses. |
| `/api/role` | Preserve supported behavior; record the existing missing membership-table defect separately. |
| `/api/communities` | Preserve current methods and authorization behavior. |

Implement versioned internal gRPC methods:

- `BatchGetProfiles`: maximum 100 IDs; names and explicit missing IDs. Email access only for authorized recipient-selection consumers.
- `BatchGetPrincipals`: existing role and community values.
- `SearchProfiles`: paginated matching IDs, preserving current name-search semantics.

Validate signed Google caller tokens, exact audiences, and method-specific allowlists. Audit receives display-name access only. Never expose password, OTP, or verification-proof material.

Replace direct user joins/preloads in Files, Admin, Forms, review jobs, Support, and activity queries. Batch requests before business transactions; preserve search, pagination, recipient selection, and authorization checks.

Identity serves public HTTP and internal gRPC through one HTTP/2 listener. See [Cloud Run HTTP/2 configuration](https://docs.cloud.google.com/run/docs/configuring/http2).

## Ownership and migration

Introduce `identity` and `audit` schemas using additive migrations after Milestone 3.

- Deploy compatibility code before relocating existing tables.
- Allow only explicitly validated transitional search paths and narrow table grants.
- Preserve physical tables, IDs, sequences, timestamps, and existing deletion behavior.
- Transfer Identity notification-outbox rows transactionally after old auth requests and dispatchers drain. Preserve delivery IDs, event keys, ciphertext, deadlines, and state.
- Give the existing worker a private Identity dispatch operation rather than auth-table access.
- Transfer the existing signup-cleanup **Cloud Run Job** to the Identity image and runtime identity. Keep one cleanup job and no cleanup API.
- Revoke transitional grants only after compatible rollback has been verified.

The planning audit found 30 foreign keys referencing users and an installed NIA view depending on communities. Preserve these as documented transitional dependencies; test them during relocation. Their eventual removal belongs to the later owner migrations.

Do not create the absent `user_roles` or `access` tables as part of this extraction.

## Audit extraction

Create private `nordikdriveapi-audit`, its restricted database role, and a separate Cloud Tasks queue/task identity. Start with minimum zero, maximum one instance, and one database connection.

Move general application activity from the existing five Auth actions and ten File actions. Support scheduling history remains with Support because it drives its workflow and recovery.

Use producer-owned audit outboxes:

- Persist immutable event UUID/version, producer, occurrence time, existing activity fields, redacted metadata, and request/trace identifiers.
- Commit mutation events with their business transaction.
- Put only event identifiers in Cloud Tasks.
- Audit fetches the event through an authenticated owner endpoint and commits ingestion idempotently.
- Confirm durable ingestion before clearing producer payloads.
- Retain failed events for recovery; an Audit outage must not undo committed business operations.

Use private HTTP/JSON for dispatch and event retrieval/acknowledgment on Legacy and Identity. Audit receives no SMTP or notification-encryption credentials.

Preserve `POST /api/logs`, historical IDs, default date range, filters, pagination, totals, and aggregates. Resolve name-matching user IDs **before** counting and pagination, then batch-enrich displayed names. Preserve separate firstname/lastname matching and existing wildcard behavior. Identity failures must produce an explicit error rather than incomplete successful results.

## Deployment capacity and coordination

Independent replacement allowances would require **24 connections**, exceeding the safeguard. Introduce serialized runtime rollouts before provisioning the new services:

| Stage | Planned allocation |
| --- | ---: |
| Completed M3 with serialized rollouts | 16 |
| Identity added | 17 |
| Identity and Audit added | 18 |

These are the approved allocations after deployment coordination is implemented; the accepted M3 allocation before that change is 20. Keep worker pool two and the existing external/NIA reservation unchanged.

Implementation prerequisites:

- Label database connections with their revision/execution.
- Add a private Worker inspection endpoint returning only aggregate connection counts from a fixed query, using its existing pool and a GitHub-deployer-only allowlist.
- Coordinate runtime changes through one persistent rollout marker on the existing gateway.
- Use conditional metadata updates and preserve unrelated configuration. Verify first that marker updates change neither traffic nor revisions. See the [Cloud Run service update API](https://docs.cloud.google.com/run/docs/reference/rest/v2/projects.locations.services/patch).
- Keep interrupted or failed windows blocked until explicit recovery.
- Release a window only after PostgreSQL sessions and both active/idle Cloud Run instances have drained. Missing telemetry is insufficient evidence.

Keep image publication independent: service-folder changes select that service; shared changes select affected consumers; documentation/tests select none. Add Identity/Audit build and deployment targets. Promote only an explicitly tested revision.

## Validation and rollout

Require regression, real PostgreSQL integration, race, Envoy, workload-identity, Terraform, and contract checks, with at least 90% coverage for handwritten Identity/Audit runtime code.

Verify:

- Existing sessions, refresh, password reset, signup OTP, and unchanged user IDs.
- Profile/search/authorization parity across every migrated consumer.
- Audit transaction rollback, queue outage, duplicate delivery, lost acknowledgment, recovery, and historical-query parity.
- Concurrent deployment refusal, interrupted-rollout blocking, drain detection, and compatible rollback.
- Controlled warm/cold behavior and an initial audit-visibility target of 120 seconds.
- Existing-session checks for files/downloads, chat, and Support/Zoom.

Deploy foundations first, Identity second, Audit third. Perform table relocation and grant removal only after their compatibility gates pass. Record accepted revisions, digests, migrations, remaining transitional dependencies, and rollback procedures.

Keep the accepted Milestone 3 schedules: normalization every six hours and reconciliation/cleanup every twelve hours. Transferring signup cleanup to Identity preserves that job and its schedule. The [Milestone 3 completion guide](milestone-3-completion.md) records the existing delivery and recovery behavior.
