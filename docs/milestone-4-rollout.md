# Milestone 4 development rollout

This runbook implements the [approved plan](milestone-4-plan.md). Source validation is separate from cloud acceptance. Do not mark the milestone complete until the final acceptance record exists.

## Scope and release record

Use project `planar-ray-472112-e8`, region `us-west1`, and the existing Terraform backend/state. Start from the accepted M3 release file `infra/terraform/milestone3-release.tfvars.json` and copy the **whole** document to ignored `infra/terraform/milestone4-release.tfvars.json`. Preserve every current image, complete settings/secret maps, JWT reference/version, task configuration and schedule. Never initialize a new backend or replace maps with partial maps.

Record the reviewed source commit, successful CI run, six immutable image digests, current serving/template revisions, state serial, migration checks and rollback revisions in an ignored release record. Read actual cloud metadata when executing each phase; the M3 revisions recorded in old documents are historical evidence, not deployment inputs.

Development keeps the database tier and external/NIA reservation unchanged. Pools remain Gateway 1, Legacy 2, Worker 2, Notifications 1, and new Identity/Audit 1 each. The pre-coordination allocation is 20; accepted serialized allocation is 16, then 17 with Identity and 18 with Audit. Do not add owner services before serialized coordination is accepted.

## 1. Validate and publish reviewed source

Run from the repository in PowerShell:

```powershell
.\scripts\test-milestone2-terraform.ps1 -SkipInit
python -m unittest discover -s scripts -p 'test_runtime_*.py'
python -m unittest discover -s scripts -p 'test_check_runtime_plan.py'
python -m unittest discover -s scripts -p 'test_changed_services.py'
.\scripts\test-milestone2.ps1 -WithEnvoy -KeepFixture
```

CI also verifies race behavior and reproducible protobuf generation, and enforces at least 90% combined handwritten Identity and Audit runtime coverage. The PostgreSQL tests run real migrations in disposable databases and test restricted roles, preserved physical relation/sequence IDs, thirty foreign keys, the NIA view, deletion behavior, historical activity queries and lost acknowledgments.

Merge only after required CI and the repository's approving review. The release workflow publishes separate images for affected services. Before the persistent marker exists, it deliberately publishes without creating runtime revisions. A service folder selects that service; shared Go dependencies select their consumers; documentation and test-only edits select none.

Keep the published artifact index and select its SHA256-verified Linux amd64 child as the runtime reference. Cloud Run reports the child digest on its revision. Job summaries record both references; provenance attestations remain published. A previous index reference can be resolved only by verifying its immutable contents and unique platform descriptor, never by substituting an unrelated digest.

Use `/health` for authenticated probes through the Cloud Run frontend; retain `/healthz` for existing container startup probes. Worker readiness uses its authenticated, timestamped aggregate database inspection instead. A failed or stale inspection must leave the window held.

## 2. Establish coordination without reducing the budget early

Install `migrations/005_runtime_inspection.sql` as the existing database installation identity, passing the existing Worker IAM database username as the psql variable `worker_user`. This creates one fixed, aggregate-only `SECURITY DEFINER` function with a pinned `pg_catalog` search path. Only Worker receives `EXECUTE`; Worker does **not** receive `pg_read_all_stats`. The function refuses to run if its installation owner lacks statistics visibility. Verify that its counts include a connection from another role and that Worker cannot read that role's SQL text.

Use the existing GitHub/WIF deployment identity for private verification through **Verify an existing development rollout window** (`verify-runtime-window.yml`). Its `test` operation verifies the exact candidate and aggregate inspection; `drain` releases an already promoted window only after complete telemetry. Supply only its target and existing window identifier. Credentials stay in runner environment; artifacts contain verification metadata only. This requires no local service-account impersonation grant. Local SDK credentials perform the reviewed Terraform and coordinator metadata/configuration operations; never save access or ID tokens in artifacts or Terraform files.

Run `python scripts/runtime_deploy.py bootstrap`. It updates only the gateway service annotation, verifies unchanged traffic/template/latest-created revision, and proves a **conflicting** stale write is refused with `etag` explicitly included in the update mask. Identical no-op requests do not establish this proof. A failed proof leaves deployment blocked; do not manually erase the marker. After correcting its cause, bootstrap recovery requires the exact existing `--token` and an explicit `--reason`; it repeats the full proof before enabling coordination.

Keep `serialized_runtime_rollouts=false` and both new services disabled for the first Worker foundation. Set `rollout_inspection_enabled=true` and `rollout_deployer_account` to the existing deployer. Prepare a Worker window and stage its configuration/image at zero traffic. Install its inspection invoker and minimal Monitoring permissions. Verify caller rejection and authenticated version-2 aggregate output. Promote the exact tested revision and observe the retired Worker drain. Then update Legacy, Notifications and Gateway **one at a time**, adding revision-labelled connections. Only after these checks set `serialized_runtime_rollouts=true`.

The initial foundation needs targeted Terraform plans because the common connection-label environment setting affects multiple services. Target the selected service and its necessary IAM resources, inspect the complete saved plan, and reject any second runtime mutation. Do not use a blanket apply.

Update the two existing finite cleanup job templates separately so their database connections also carry execution labels. Pause the selected job's existing Scheduler, prove every old execution terminal and its SQL sessions gone, then prepare a window for that job's owner (`gateway` for limit cleanup, `identity` for signup cleanup). Review a saved **one existing job, zero services** plan with `check_runtime_plan.py --job-only --target <owner>`. Apply it, verify its image/pool/arguments, record the check and close with `close-no-replacement` because no service revision was replaced. Resume the same Scheduler after a controlled finite execution and session-drain check; do not create another job or overlap executions. Signup image/account transfer may instead accompany its later Identity configuration window after the same execution gates. Run a full reconciliation plan after the sequential foundation is complete.

## The existing-service configuration window

Use this same sequence whenever a later phase changes an existing service's environment, identity, schema path or image:

1. `python scripts/runtime_deploy.py prepare --target worker --image <reviewed-worker-digest> --owner <release-record-id> --output .cache/m4-window.json` (substitute the actual target and its image). This persists the intended revision and snapshots of all six runtimes before mutation.
2. Copy the returned token into `rollout_window_token`. Pin all verified serving revisions in `runtime_serving_revisions` and the public gateway in `public_serving_revision`. Put only this target's returned revision in `runtime_candidate_revisions`. Preserve all other values. Keep `runtime_template_revisions` for previously accepted templates.
3. Save a Terraform plan against the existing state, using both `nordikdrive.tfvars` and the complete M4 release file. Inspect its JSON with `python scripts/check_runtime_plan.py <saved-plan.json> --target worker`. Review metadata/IAM/database grants too. The guard rejects deletion, billing, wrong environments, public IAM and any second runtime mutation. Apply only that exact reviewed plan.
4. `python scripts/runtime_deploy.py record-stage --token <window-token>`, then dispatch the WIF verification workflow with operation `test`, the same target and window identifier. Require its success. Run the phase's authenticated compatibility/contract checks against the candidate URL before promotion. A health probe alone does not establish business acceptance.
5. `python scripts/runtime_deploy.py promote --token <window-token>`, then dispatch the WIF verification workflow with operation `drain` and the same target/window. Promotion removes the candidate tag and pins 100% to the tested revision. Require successful workflow drain verification; it can be resumed against the same held window after interruption.
6. The coordinator releases only after fresh SQL counts and explicit **both active and idle** Cloud Run instance counts prove the retired revision has drained. For old unlabelled M3 revisions it also requires zero owner-scoped unlabelled sessions. Missing/stale telemetry cannot release the window. Record the accepted serving/template revision and retain its template name in `runtime_template_revisions`; clear only `runtime_candidate_revisions` after promotion.

For image-only releases the deployment workflow performs this sequence automatically under the persistent marker and one global queue. Image publication remains independent. An interrupted window blocks later deployments until explicit recovery.

## 3. Provision Identity privately and migrate consumers

Apply additive `005_identity_audit_foundations.up.sql` without relocating tables. Prepare the new Identity account/Cloud SQL IAM user and narrow grants in `005_identity_audit_compatibility_grants.sql`. Audit grants are a separate script applied when that user is registered in its later phase. Use existing migration privileges; no broad runtime grants or passwords are needed.

Extend the full release secret map with `identity.NOTIFICATION_PAYLOAD_KEYS` and `identity.SIGNUP_VERIFICATION_KEYS`, reusing the existing numeric secret versions. Identity and Audit receive the unchanged JWT through `jwt_secret`. Identity receives no SMTP secret; Audit receives neither SMTP nor notification-encryption secrets.

Set `enable_identity=true`, pin its image, retain accepted M3 delivery and verified signup settings, and initially leave consumers/public routing disabled. Use `prepare-new --target identity --image <digest> --owner <record>` before provisioning. Set its returned token in `rollout_window_token` and its returned revision in `runtime_template_revisions.identity`; do not add the absent service to serving/candidate maps. Review the saved one-service plan with `check_runtime_plan.py --target identity` before applying. New private service creation has no retired revision; do not invent drain evidence. After exact revision/image readiness and owner RPC/auth checks, run `record-stage`, require the WIF `test` operation, and run `finish-provisioning` with its token. This verifies prior absence and unchanged other runtimes. Record its accepted serving revision before the next replacement.

Verify Identity's shared HTTP/2 listener: original signed caller tokens, exact audience, maximum 100-ID batches, explicit missing IDs, profile/search parity, and denial of Audit email/principal requests. Private owner services must have no public IAM principal.

Enable `enable_identity_consumers=true`. Sequentially deploy Legacy and Worker with their owner API clients and Notifications with both private outbox sources. Resolve and cache profiles/recipients before business transactions. Test Files, Admin, Forms, review jobs, Support/Zoom, search and owner-unavailable errors while the tables still remain in `public`.

## 4. Move public Identity traffic and relocate ownership

Set `identity_public_enabled=true`, update Gateway routing in its own window, and confirm existing cookies, refresh, signup OTP, password reset, roles and community behavior on the unchanged public API URL. Then update Legacy to disable its old Identity routes and drain that revision. Set `identity_outbox_exclusive=true` only during this controlled cutover; preserve other notification producers and recovery identities.

Transfer the **existing** signup-cleanup Job using `identity_signup_cleanup=true`. Pause its existing scheduler during transfer and wait for old executions to finish. Before relocation the Identity image uses `identity,public`; after relocation cleanup uses `identity`. Keep exactly one finite job, one connection and the accepted twelve-hour schedule. Do not add a cleanup endpoint or second job.

After old auth requests, old dispatchers and old cleanup executions have drained, execute `006_identity_relocate.sql` with `compatibility_verified=true` and the verified Legacy/Worker usernames. It relocates the physical relations/sequences and transfers complete encrypted Identity notification-outbox rows in one transaction. Never regenerate delivery IDs, keys, ciphertext, deadlines or states. Verify preserved OIDs/IDs, thirty foreign keys, NIA view and both deletion behaviors against the pre-migration inventory. Then record `identity_tables_relocated=true` and accept the compatible runtime paths sequentially.

## 5. Provision Audit and switch producers and queries

Register Audit's IAM database user and apply `005_audit_compatibility_grants.sql` with its verified `audit_user`. Register the private Audit runtime and separate task identity/queue with `enable_audit=true`; start the queue paused, producers disabled and public queries on Legacy. Provision through `prepare-new`, setting its returned token and `runtime_template_revisions.audit` as for Identity, followed by exact readiness/authorization tests and `finish-provisioning`. Audit's profile grant has display names only. Its sole secret is the existing JWT.

Enable `enable_audit_events=true` and sequentially deploy Identity and Legacy producer outboxes plus Worker private reconciliation. Verify both private owner read/ack paths and ingestion identity. Mutation failure must roll back its audit intent; a queue/Audit outage must leave committed business state and pending intent intact. Only event identifiers appear in tasks. Test duplicate delivery, lost acknowledgment and recovery; clear payload only after committed Audit ingestion is acknowledged.

Set `audit_queues_enabled=true` after source and ingestion checks. Immediate publication provides normal activity visibility; reconciliation uses `35 */12 * * *`. Confirm a controlled new Auth and File activity becomes visible within the initial 120-second target. Do not change Support's workflow history.

Set `audit_public_enabled=true`, update Gateway and then retire Legacy's old activity route in separate windows. Compare `POST /api/logs` history, filters, default dates, name wildcards, totals, pagination and aggregates. An Identity failure must return an explicit error rather than a successful incomplete page. After compatible queries/producers and old revision drains are accepted, run `007_audit_relocate.sql` with `compatibility_verified=true`, then record `audit_tables_relocated=true`.

## 6. Compatible rollback, grant removal and final acceptance

Keep transitional grants until exact rollback images have been tested with the current owner APIs and schemas. Rollback after extraction must not restore direct user/log access. Record the known missing `user_roles`/`access` defect separately; do not create those tables.

If a window fails, inspect `python scripts/runtime_deploy.py describe`. Retry/recover the same recorded candidate with `recover --token <token> --reason <reason>`; no window expires automatically. A prepared window with no revision can close only with `close-no-replacement --token <token> --reason <reason>`, which proves all runtime snapshots unchanged and the planned revision absent.

To restore a compatible accepted revision, create an ignored evidence JSON file containing its exact `target`, `revision`, immutable `image`, current `schema_phase` (`m3`, `identity_active`, `identity_relocated`, `audit_active`, `audit_relocated` or `grants_removed`) and successful `public_contract_verified`, `cookies_verified`, `rollback_checks_passed`, plus `owner_api_verified` and `audit_outbox_verified` where applicable. Run `rollback --token <token> --evidence-file <file> --reason <reason>`, `test`, `promote`, and `drain` with the same token. This restores traffic to the accepted existing revision without creating a third revision. It retains a record of the failed last-created template so the next release reconstructs the accepted configuration. Never clear a failed marker to force another deployment.

After compatible rollback acceptance, execute `008_identity_audit_revoke_transitional.sql` with `compatible_rollback_verified=true` and the verified role names. Confirm Legacy/Worker can no longer query Identity tables, old activity grants are removed, and owner APIs, foreign-key checks and the installed NIA view still work. Record those foreign keys and the NIA view as remaining transitional dependencies for later owner migrations.

Complete controlled warm/cold and caller-policy checks and the user's existing-session checks for login, files/downloads, chat and Support/Zoom. Preserve normalization every six hours, other reconciliation/cleanup every twelve hours, and existing daily business schedules. Save accepted revisions/digests, migrations, grants, budgets, measured activity visibility, remaining dependencies and compatible rollback references. Only this cloud evidence establishes Milestone 4 completion.
