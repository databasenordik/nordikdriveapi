# Milestone 3 completion rollout

This is the remaining development rollout after PR #137. The earlier release guide records the initial Notifications/signup deployment. Keep the frontend, JWT, keys, public URLs, database tier, and production unchanged while preparing this update.

## Result and reason

New email intents commit with the business operation. Before the HTTP or job handler returns, the producer publishes an ID-only Cloud Task to the existing OTP or normal queue. Notifications retrieves the encrypted command through the private producer endpoint, records delivery durably, sends it, and acknowledges the outcome. Publication failure leaves the intent available for automatic reconciliation. No goroutine needs to stay alive between requests.

Cloud Tasks retries confirmed transient failures with bounded backoff and the existing provider-attempt limit. A completed delivery is not sent again on redelivery. Uncertain SMTP acceptance remains `unknown` and is never automatically resent. Operator replay remains disabled; daily operation requires no manual replay workflow.

Replace/revert operations also commit a file-version work item. A new ID-only task triggers processing on the existing Worker immediately after commit. Existing leases, transaction checks, and attempt limits protect redelivery. This uses one restricted work queue/task identity and the existing Worker pool, with no new service or database pool.

Normalization transforms raw uploaded rows into the normalized data used by chat. It runs every six hours, including missing/changed older rows. It does not run inline on upload.

## Final schedules

| Work | Schedule | Time zone |
| --- | --- | --- |
| Notification repair/reconciliation | `0 */12 * * *` | UTC |
| File review email reconciliation | `5 */12 * * *` | UTC |
| Form review email reconciliation | `10 */12 * * *` | UTC |
| File normalization | `15 */6 * * *` | UTC |
| File-version reconciliation fallback | `20 */12 * * *` | UTC |
| Gateway rate-limit cleanup | `25 */12 * * *` | UTC |
| Signup expiry cleanup | `30 */12 * * *` | UTC |
| Daily honour and Support maintenance | Existing midnight daily | America/Toronto |

The staggered scans allow runtimes to scale to zero between requests. First delivery and file-version processing use tasks independently of these scans. Delayed normalized-data availability is accepted for development.

## 1. Validate and merge the source

Run regression, PostgreSQL integration, Envoy, static checks, Linux race checks, service-selection tests, and Terraform mocks. Notifications must retain at least 90% combined coverage. Review private caller boundaries, retry exhaustion, transaction rollback, and unknown-send behavior.

Merge after required GitHub review and CI pass. The dependency-based release workflow builds and stages affected services without moving traffic. Retain immutable digests. Documentation/test-only changes select no service; shared dependencies select their affected consumers.

Why: deploy the event paths before slowing fallback scans.

## 2. Prepare the private source and callbacks

Use the existing development Terraform state and complete ignored values in `infra/terraform/nordikdrive.tfvars` and `infra/terraform/milestone3-release.tfvars.json`. Preserve complete secret/settings maps and serving revision pins.

Set `enable_notification_source=true`. Initially leave `enable_event_driven_notifications`, `enable_file_version_events`, `enable_low_frequency_reconciliation`, and `enable_signup_cleanup` false. Select reviewed server, Worker, and Notifications digests; retain the gateway image. Review a saved plan for development resources only, no deletions/replacements, and the existing capacity safeguard.

Stage with traffic pinned. Verify and promote Legacy's private source first, then Notifications' source client/receipts. Only Notifications may read sealed intents or acknowledge outcomes. Ordinary production and frequent scans continue during this stage.

Why: delivery tasks must not reach a missing source endpoint. Secret values need no changes.

## 3. Enable immediate email and file processing

Set `enable_event_driven_notifications=true` and `enable_file_version_events=true`, keeping `enable_low_frequency_reconciliation=false`. Existing notification delivery/queues remain enabled. Review queue-scoped enqueue/actAs grants and the restricted file-work queue/task identity. The latter has no SQL user and cannot invoke scheduled jobs at the application layer.

Stage and verify Worker first, including its file-task handler and separate signed Scheduler authorization. Promote it before Legacy starts publishing file tasks. Then verify/promote Legacy with both publishers enabled. Keep the verified Notifications revision serving.

Use controlled development tests to prove immediate OTP/normal delivery, sealed-source retrieval, durable acknowledgment, file replace/revert processing, duplicate suppression, and signed caller/audience rejection. Record outcomes without credentials, recipients, codes, proofs, or payloads. Verify warm delivery and delivery after Notifications reaches zero instances.

Why: retain frequent scans until immediate processing passes cloud acceptance.

## 4. Activate finite cleanup and slower scans

Take the normal development backup and apply additive `004_signup_cleanup.up.sql` through the established migration/owner connection. Its expiry indexes use bounded lock/statement timeouts; startup never migrates.

Set `enable_signup_cleanup=true` and `enable_low_frequency_reconciliation=true`. Review a saved plan that provisions the finite cleanup job with the reviewed server digest and changes schedules. The job uses the Legacy identity, one connection, no JWT/SMTP/signup key environment variables, bounded batches, and a singleton advisory lock. It deletes only expired auth state. Users and notification deduplication records remain intact. Planned allocation becomes 20 of the existing 20-connection safeguard.

Apply the reviewed plan. Execute cleanup once for acceptance and inspect its count-only result. Verify the final schedules, minimum zero instances, unchanged daily jobs, and the old `honour-daily` job still paused.

Why: expiry cleanup belongs in a finite Cloud Run Job; APIs do not perform a sweep.

## 5. Accept and retain compatible rollback

Retest existing login/cookies, uploads/downloads, chat, and Support/Zoom. Record serving revisions, digests, migration/index status, SQL usage, outcomes, cold-start measurements, and schedules in the verification report. Milestone 3 acceptance remains open until these pass.

If immediate publishing fails, restore frequent fallback scans before disabling publishers/source. Keep the source available while event tasks or receipts remain outstanding. Do not restore raw SMTP producers while queued deliveries can send. Retain additive migration 004 and deduplication records during rollback.

Allow prior revisions and SQL sessions to drain before staging another revision of the same service. A second unused warm revision is outside the single replacement allowance; missing telemetry is not drain evidence.
