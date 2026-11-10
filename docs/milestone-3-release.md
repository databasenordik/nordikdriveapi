# Milestone 3 implementation and development release

The implementation is prepared locally. It has not changed Cloud Run, SQL, Secret Manager, queues, traffic, billing, or production. Cloud acceptance is still required before calling the milestone deployed.

[Local verification results](milestone-3-verification.md) record the checks already completed and the remaining cloud acceptance gates.

## What is implemented

`cmd/notifications` is a separate private service. It owns encrypted delivery records, Cloud Tasks dispatch, bounded retries, Gmail SMTP, immutable Support screenshots, and audited operator replay. Auth, Records, and Support still select recipients and render their existing messages. Business updates and email intents commit together. An uncertain SMTP result becomes `unknown` and is never automatically resent.

Signup now has these contracts:

| Request | Result | Purpose |
| --- | --- | --- |
| `POST /api/user/signup/otp/request`, body `{"email":"..."}` | `202`, challenge ID, expiry, resend time | Send or resend a six-digit signup code without creating an account. |
| `POST /api/user/signup/otp/verify`, body `{"email":"...","challenge_id":"...","otp":"012345"}` | `200`, `signup_verification_token`, expiry | Verify ownership without creating an account or session. |
| Existing `POST /api/user/signup`, existing fields plus `signup_verification_token` | Existing `201` account response | Consume the proof and create the account/welcome intent in one transaction. |

Use `Idempotency-Key` for all three requests. A retry after a lost response uses the same key and body; a deliberate resend/new code attempt uses a new key. Codes and proofs expire in ten minutes. Resend waits sixty seconds and invalidates earlier codes/proofs. Five wrong codes exhaust a challenge; persistent email/IP limits also apply. Existing accounts, password-reset routes, JWT values, and login cookies retain their current behavior.

The companion frontend changes are in `C:\Nordik Projects\nordik-drive`. The proof stays in memory, editing the email clears it, and neither community nor account creation runs before verification. Both repositories must be reviewed for this release.

## How independent releases work

[services.json](../.github/services.json) lists four independent images and Dockerfiles. [changed_services.py](../scripts/changed_services.py) reads each executable's Linux Go dependency graph before selecting the build and staging matrix.

| Changed runtime source | Images selected |
| --- | --- |
| `internal/notifications/**`, `cmd/notifications/**` | Notifications only |
| A service's Dockerfile | That service only |
| Shared Go package | Only executables importing that package, directly or transitively |
| `go.mod`, `go.sum`, `.dockerignore`, release workflow/manifest/selector | All four, because these are common build/release inputs |
| Documentation, tests, Terraform only | No application image release |

Some domain packages are still shared by legacy and worker, so changing those packages correctly selects both. Merely changing a file in an unrelated folder does not select Notifications. Unit/integration CI still checks the repository; these rules control image publication and service staging.

The `nordik_nia_api` repository's existing automatic release now has equivalent filters for its actual TypeScript `backend` and shared build inputs. Its imported runtime Markdown remains a build input; README/CHANGELOG files and documentation directories are excluded. Its explicit manual/tag releases remain explicit operator actions. No NIA deployment commands, research service settings, or runtime configuration changed.

After merge to API `master`, **Release changed development services** publishes immutable digests and stages only selected services with `--no-traffic`. Manual staging remains available as an explicit override. For the first Notifications image, staging reports that Terraform provisioning is needed if the service does not exist. It does not create a service with incomplete configuration. The gateway keeps its existing public URL. A source merge does not promote a revision into live traffic.

## Release sequence and reasons

### 1. Review and validate the release source

In PowerShell at `C:\Nordik Projects\nordikdriveapi`, review `git status --short` and the Milestone 3 diff. Include the migrations, service Dockerfiles, workflows, selector, backend, Terraform, tests, and documentation. Review the companion frontend and the NIA workflow filter separately. Do not include ignored `.cache` files, credentials, or unrelated work.

Required checks are defined in [ci-go.yml](../.github/workflows/ci-go.yml): Go regression/integration, real Envoy routing, static/race checks, service-selection tests, Terraform mocks, and 90% coverage for each new platform core. The existing local `scripts/test-milestone2-terraform.ps1` also runs the new Notifications mocks. Frontend verification:

```powershell
Set-Location 'C:\Nordik Projects\nordik-drive'
$env:CI = 'true'
npm run test:unit -- --watchAll=false --runInBand --runTestsByPath src/hooks/useSignupVerification.test.tsx src/pages/auth/Signup.test.tsx --testTimeout=30000
```

Merge the API release only after its required review/CI. Retain the four image digests from Actions. Keep the frontend release controlled until the OTP backend is active. The NIA workflow-only change can be reviewed independently; merging a change to that workflow intentionally triggers its own existing deployment pipeline.

**Why:** The reviewed source, images, and rollout values must describe the same release. Local test success does not establish cloud acceptance.

### 2. Recheck the current development deployment

Record serving revisions/traffic and image digests for the gateway, legacy, worker, and development NIA. Recheck every database consumer, configured pools/instance limits, IAM, SMTP secret reference, schedules, and current email collisions. Use read-only inventory. Do not print environment secret values. The recorded 19-of-20 connection proposal is not a fresh capacity measurement.

**Why:** A previous inventory cannot establish today's safe allocation. Keep the existing 80% safeguard, production, other applications, billing, project, bucket, database tier, network, and Terraform state unchanged.

### 3. Prepare only the new keys and reviewed Terraform values

Use the current `infra/terraform` state and existing development values; do not reinitialize into a new state or reuse test fixtures as deployment values. Keep existing public revision/traffic pins and current image digests for any service not intentionally being released.

Create two independent 32-byte random keys: one for signup verification, one for notification payload protection. Each secret payload is a JSON version map, for example `{"1":"BASE64_OF_32_RANDOM_BYTES"}`. Store them in Secret Manager without logging payloads. Retain old logical key versions through outstanding challenge, idempotency, and delivery lifetimes. These keys do not replace JWT. Reuse the existing SMTP password secret; `GMAIL_USER` may remain a nonsecret setting. Import any manually created secret metadata into its existing Terraform resource address before planning it.

Relevant Terraform inputs:

| Input | Initial delivery test | Producer cutover | Signup enforcement |
| --- | --- | --- | --- |
| `enable_notifications` | `true` | `true` | `true` |
| `project_number` | Verified project number, `724838782318` in the recorded deployment | Same verified value | Same verified value |
| `notifications_image` | Reviewed `nordik_notifications_go@sha256:...` | Keep verified digest | Keep verified digest |
| `notification_delivery_enabled` | `false` | `true` | `true` |
| `notification_queues_enabled` | Initially `false`, then `true` for controlled delivery | `true` | `true` |
| `signup_require_verified_email` | `false` | `false` | `true` after compatible frontend |

Pin numeric Secret Manager versions through `runtime_secrets`: payload keys for Notifications/legacy/worker, signup keys for legacy only, SMTP password for Notifications. Set Notifications' Gmail username in `runtime_settings.notifications`. The logical key-map selector inputs default to `"1"`; these are distinct from Secret Manager version numbers. Select the dedicated worker digest through `worker_image`; retain current server/adapter digests until their planned rollout.

Review a saved Terraform plan using the existing development var files plus the reviewed Milestone 3 values. Stop if it changes production/unrelated applications, replaces/deletes resources, changes billing, or exceeds verified capacity. Apply only the reviewed saved plan. Notifications starts with one SQL connection, concurrency one, minimum zero, and a bounded replacement allowance; both queues and the added dispatcher schedule start paused until enabled.

**Why:** This provisions one private service, two queues, its narrow identities/grants, and one recovery schedule without activating business producers prematurely.

### 4. Apply the additive database migration and grants

Take the normal development backup first. Use the established owner/migration connection and PostgreSQL 15 client, with bounded SQL lock timeouts already in the migration. Run [003_notifications.up.sql](../migrations/003_notifications.up.sql), then [003_notifications_grants.sql](../migrations/003_notifications_grants.sql). Supply the three `psql` variables from Terraform's `database_users` output: `notifications_user`, `legacy_user`, `worker_user`. The IAM database users must already exist. Do not run the grants file in Cloud SQL Studio unchanged: its `:"variable"` syntax is processed by `psql`.

Notifications' health endpoint can start before its tables are ready, so a healthy revision alone is not evidence that this step is complete. Check the tables, legacy-only auth grants, Notifications-only delivery grants, and unchanged existing accounts before making intake requests. Application startup never runs migrations.

**Why:** This adds compatible persistence while keeping Notifications out of user/business tables and worker out of signup challenge/proof state.

### 5. Prove private delivery before switching producers

Use controlled recipients. Verify Cloud Run IAM and application caller roles separately: legacy/worker can submit allowed commands; the dedicated task identity can deliver; neither can approve operator replay. Prove the original signed-token convention with a real Cloud Tasks dispatch. A platform-authenticated request that fails application token verification is a failed rollout gate; never accept unsigned claims as a workaround.

Resume the queues only for controlled tests. Prove deduplication, provider acceptance, immutable screenshot downloads, safe retry limits, enqueue recovery, `unknown` outcomes, and wake-up from zero. Verify the one-connection pool releases SQL before SMTP/Tasks/storage calls. Use structured `notification_intake`, `notification_enqueue_failed`, `notification_delivery`, and `notification_outcome_write_failed` logs; do not log recipients, codes, proofs, bodies, attachments, credentials, or raw provider errors. Inspect queue age and terminal/unknown states before proceeding.

**Why:** These are cloud-specific contracts that local mocks cannot establish. The proposed acceptance targets are 60 seconds warm and 120 seconds cold, measured to provider acceptance rather than inbox arrival.

### 6. Switch legacy and worker, then the frontend and signup enforcement

Stage/promote the reviewed legacy and dedicated worker revisions together with `notification_delivery_enabled=true`; keep the gateway's existing public service and verified routing. This is one runtime-wide transport switch, not separate per-producer toggles. Enable the dispatcher schedule after the worker has its `notification-outbox-dispatch` handler. Validate welcome/reset/password-change, file/form review messages, Support recipients/screenshots, and persisted Zoom links. Queued mail must not mark business flags successful; provider acceptance does.

With signup keys configured and outbox mode active, test both OTP endpoints through the real gateway, including browser preflights with `Idempotency-Key`. Release the compatible frontend, then set `signup_require_verified_email=true` and promote that legacy configuration. Prove that no account exists after request/verify, final signup creates exactly one verified account, direct signup without proof fails, and subsequent login works. Exercise wrong codes, expiry, resend, changed email, duplicate clicks, and lost responses.

**Why:** Enforcing proof before the frontend supports it would block new registrations. Releasing only frontend checks would leave direct API signup unprotected.

### 7. Record acceptance and keep a compatible rollback

Retest existing login/cookies, uploads/downloads, chat, Support/Zoom, and scheduled jobs. Record reviewed commits, immutable images, serving revisions, schema/grant status, secret version numbers, observed SQL usage, and delivery outcomes. Live delivery and browser acceptance close the milestone.

Keep outbox-capable rollback revisions and required key versions. Pause the affected queue/producer rollout and inspect pending/unknown records before replay. Do not restore raw SMTP producers while queued intents can still send. The operator-only replay API requires a reason and idempotency key, records the verified caller, and grants exactly one additional attempt; it cannot resend OTPs or expired messages. Retain additive tables during rollback. After mandatory signup activates, a frontend rollback must still support verification; otherwise pause new registrations while repairing it.
