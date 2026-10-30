# Monolith Baseline

Recorded on 2026-10-30 for step 1 of the [migration roadmap](micro_service_architecture.md#implementation-plan). This is a source and synthetic-test baseline, not an inspection of the deployed system. No production data, cloud configuration, historical logs, or credentials were changed.

## Changes in This Step

- Auth audit events now send explicit account fields rather than whole requests. The audit storage boundary recursively redacts credential keys in structured metadata, preserves numeric precision, and omits invalid/unstructured payloads without losing the event. It copies input before redacting. Free-text messages still require deliberate safe field selection.
- Middleware, `/me`, and refresh share `internal/session` validation: HS256, required expiration, a configured key, and positive decimal integer user IDs within both the platform integer and JavaScript-safe integer range. Invalid claims return 401 rather than causing a type-assertion panic. Cookie names, security flags, token lifetimes, response fields, and the middleware's float64 context value remain compatible. Issuer, audience, and access/refresh token separation need a future coordinated token migration.
- Startup validates `JWT_SECRET` before opening dependencies, checks the Vertex constructor error before wiring handlers or starting jobs, and closes the SQL pool on initialization failure and server exit. Startup diagnostics omit raw credential-bearing errors. SQL diagnostics retain query placeholders rather than parameter values. Client construction is not a live Vertex readiness check.
- CI uses the Go version from `go.mod`. Three previously undiscovered integration files are renamed and repaired, retaining their 31 cases. Each SQLite fixture is isolated and closed. Tagged PostgreSQL tests and combined coverage now have a dedicated local/CI fixture.

## Reproduce Validation

Use Go 1.25 and a clean checkout. Ignored local `cmd/deceasedimport/` and `internal/deceasedimport/` tools are outside the repository baseline. Do not use production connection settings or set `NIA_TEST_TARGET` to a production service.

```sh
make test-unit
docker compose -p nordik-baseline -f docker-compose.test.yml up -d --wait
export TEST_DATABASE_URL='postgres://nordik_test:synthetic-test-only@127.0.0.1:55432/nordik_baseline_test?sslmode=disable'
make test-integration
go tool cover -func=coverage.integration.out
go vet ./...
go build -o nordik-drive-api ./cmd/server
docker compose -p nordik-baseline -f docker-compose.test.yml down
```

In PowerShell, use `$env:TEST_DATABASE_URL='...'`. Without Make, use `go test ./... -count=1 '-coverprofile=coverage.unit.out'` and `go test -tags=integration ./... -count=1 '-coverpkg=./...' '-coverprofile=coverage.integration.out'`. Quote flags containing filenames in PowerShell. A Windows executable cleanup lock can be avoided with `go test -work ...`; Go prints the retained temporary directory.

The database container binds only to `127.0.0.1:55432`, has synthetic credentials and temporary storage, and uses a separate Compose project. PostgreSQL tests require a loopback host, database `nordik_baseline_test`, and user `nordik_test`; they never fall back to `DB_*`. Each test creates a random schema, sets its connection search path, closes its pool, and drops only that schema. Selecting the PostgreSQL suite without the fixture fails rather than skips. PostgreSQL 15 matches the development configuration; the deployed Cloud SQL version still needs verification.

The tagged command includes ordinary tests and restored SQLite tests in one coverage profile; do not average percentages from different runs. Generated NIA bindings are included in the raw repository percentage. The future 90% requirement applies per extracted service's handwritten runtime code, not to the current monolith.

### Verification Results

Validated on Windows/amd64 using Go 1.25.5 and PostgreSQL 15.19 in Docker Desktop:

| Check | Result |
| --- | --- |
| Default suite | 474 top-level tests pass; 2 optional Python NIA checks skip. |
| Full integration-tagged suite | 510 top-level tests pass (824 pass results including subtests); the same 2 optional checks skip. Includes all 31 restored legacy cases and 5 PostgreSQL scenarios. |
| Default coverage | **67.4%**, reported by `go tool cover -func=coverage.unit.out`. |
| Combined coverage | **68.6%**, reported by `go tool cover -func=coverage.integration.out`; **69.0%** when generated NIA bindings are excluded before reporting. |
| Static checks and server build | `go vet` across tracked repository packages and `go build ./cmd/server` pass. |
| Fixture safeguards and cleanup | Missing and nonlocal fixture URLs fail as expected. Zero owned schemas remain after testing; the temporary container and network were removed. |

Local package selection excluded the two ignored deceased-import tools; CI uses a clean checkout. Normal Go cache access was required because sandboxed runs stalled and reported denied cache/telemetry access. One Windows executable cleanup lock was avoided using `-work`. These environment workarounds do not change assertions.

`TestNIAChatController_PythonIntegration` and `TestNIAUserFileHistory_PythonIntegration` need `NIA_TEST_TARGET` pointing to a separate fixture-backed Python server. They were not executed against NIA or production. Local in-process gRPC tests pass. No new tests are skipped to obtain the baseline, and CI execution itself has not been observed from this workspace.

### Repeatable Performance Checks

With the same local fixture configured:

```sh
go test -tags=integration ./tests/integration -run '^$' -bench '^BenchmarkPostgres_' -benchmem -count=3
```

Benchmarks exercise file listing and audit search with a small, fixed synthetic dataset. Keep setup outside timing and record the Go/PostgreSQL versions, host, fixture size, and three samples. These are service/database timings on a local Docker instance, not HTTP latency, production capacity, connection-wait instrumentation, or a gateway cost forecast. Reuse the API contract scenarios below before and after extraction; collect cold-start and realistic-size/load measurements in a separately approved environment.

Recorded samples on the environment above, Intel Core i7-1365U, 12 logical processors, `-benchtime=1s -count=3`, without concurrent test load:

| Operation | Synthetic data | Three samples (ms/op) |
| --- | --- | --- |
| File listing | One user, one public file, no access grants | 1.32, 1.38, 1.63 |
| Audit search | One user, one file, one log, community filter and aggregates | 9.26, 9.70, 13.71 |

Fixtures and migrations are excluded from timing. These deliberately small samples establish a reproducible smoke baseline; they do not size the target microservices.

## Data Ownership and Transaction Inventory

All current modules still share one database identity. The owners below describe the target, not grants already in place.

| Target owner | Tables / current access | Boundaries to preserve or replace |
| --- | --- | --- |
| Identity | `users`, `otps`, `roles`, `user_roles`, `communities`; legacy `access` model | Auth writes users/OTPs; role and community services read/write their tables. Check actual use of legacy `access` before migration. Other modules read profiles, roles, and memberships directly today. |
| Records: files | `file`, `file_data`, `file_data_normalized`, `file_access`, `file_version`, `file_version_reconciliation_jobs`, `file_row_lineage` | Upload, replace, revert, normalization and reconciliation write these tables. Replace/revert use transactions and file locks; reconciliation also updates row references in forms, edit requests and stories. Retain these operations within Records. |
| Records: review/media | `file_edit_request`, `file_edit_request_details`, `file_edit_request_photos`, `file_row_achiever_stories` | File review writes request decisions and record/media state in transactions; admin reads/searches/downloads these records. Listing/history/access/admin queries join `users`; replace with batched Identity APIs during extraction. |
| Records: forms | `form_submissions`, `form_submission_details`, `form_submission_uploads`, `form_file_mappings` | Submission upsert/review transactions span header, details and uploads. File replacement checks open submissions. Form responses preload users. Keep the file/form invariant local. |
| Records: metadata | `data_config`, `file_logo_content`, `provinces`, `day_schools`, `indian_hospitals`, `file_daily_honours` | Configuration, lookup, content and honour services read these and file data. Honour generation locks candidate files in transactions and calls the current Chat AI adapter. |
| Support | `support_requests`, `support_schedule_settings`, `support_team_members`, `support_assignments`, `support_staff_availabilities`, `support_call_requests`, `support_calls`, `support_schedule_audit_logs` | Support reads user profiles/roles. Assignment, request decisions, calls, availability and Zoom synchronization use transactions and row locks. SMTP/Zoom effects cannot be made atomic by a SQL transaction; retain retry/reconciliation state. |
| Audit | `logs` | Event inserts are separate from business transactions. Search joins `users`; array overlap and lateral `unnest` drive filters/aggregates. Diagnostics do not replace these records. |
| Chat / external NIA | Go Chat reads `file`, `file_data`, normalized data and configuration; Python NIA owns conversations | Both retrieval paths must move behind Records APIs. NIA's current database functions still cross ownership boundaries. Its schema and provider-secret inventory require companion work in the NIA repository. |
| Notifications | No independent delivery ledger yet; SMTP called from auth, Records, Support and jobs | Business owners select recipients/events. Delivery will need outbox identifiers and deduplication without reading producer tables directly. |

Source references: `internal/file/service.go`, `operation_guard.go`, `version_reconciliation.go`; `internal/formsubmission/service.go`; `internal/admin/service.go`; `internal/supportschedule/{service,workflow,zoom_sync}.go`; `internal/logs/service.go`; `internal/chat/{database_router,structured_query_engine,nia_backend}.go`.

The PostgreSQL baseline uses explicit numeric `file_access.user_id` DDL: the legacy Go model declares that column as JSON despite numeric joins. Verify the deployed column type before using ORM migrations; this step does not alter production schemas.

## Jobs and External Dependencies

`internal/jobs/schedular.go` registers all six jobs per application instance, using `APP_TIMEZONE` (default `America/Toronto`). Startup also runs support maintenance.

| Job | Schedule | Effects and concurrency concern |
| --- | --- | --- |
| File-edit review email | Every 5 minutes | Reads requests/details/users, sends SMTP, then marks delivery. No cross-replica claim is evident; sending can succeed before marking fails. |
| Form review email | Every 5 minutes | Reads submissions/users/uploads, sends SMTP, then marks delivery; same duplicate-delivery window. |
| File normalization | Every 10 minutes | Reads source records and writes normalized rows; preserve progress/idempotency and bound batches. |
| File-version reconciliation | Every minute | Claims/reconciles version transitions and relinks record/form references; existing database locking must survive migration. |
| Daily honour | Midnight | Selects/locks candidate file state, calls AI, persists daily results. The shared-secret HTTP trigger also invokes this workflow. |
| Support maintenance | Midnight and startup | Extends a rolling 14-day horizon, repairs availability/assignments, and reconciles calls. Preserve existing checks rather than assuming all jobs lack guards. |

In-process cron needs a running instance and does not guarantee execution on request-billed Cloud Run at zero replicas. Move triggers to durable external scheduling in step 2, with owner-specific handlers and idempotency, before parallel service deployments.

External systems currently include PostgreSQL via GORM, GCS for uploaded media/documents, Vertex AI through ADC, Gmail SMTP for email, Zoom OAuth/meetings when enabled, and NIA gRPC when enabled. They require separate failure budgets and credentials. PostgreSQL/GCS/SMTP/Zoom effects are not a single atomic commit.

## Credential Lifecycle Inventory

Names only; no secret values or environment-file contents were read into this document. Human owners and actual deployed grants/expiry still require an operator inventory.

| Credential / issuer | Current consumers and lifecycle | Required renewal / revocation design |
| --- | --- | --- |
| `JWT_SECRET` / application operator | Auth issues HS256 cookies; middleware, `/me`, refresh validate them. Access 15 minutes, refresh 24 hours or 30 days with remember-me. Key has no enforced expiry or multi-key rotation. | Identity owns key versions and a compatible rotation window. Current access/refresh token kinds are not distinguished. Key replacement invalidates all old tokens. |
| `DB_USER`, `DB_PASSWORD` / Cloud SQL PostgreSQL operator | All monolith feature packages share the startup pool. No automatic password expiration or refresh in current code. Pool lifetime 30 minutes, max 25 open / 10 idle. | Step 2: per-service IAM DB users and connector-managed short-lived login tokens; restrict schema grants separately and document revocation of established sessions. |
| Google ADC / Google identity | Server Vertex client and GCS helpers. Runtime service-account attachment and actual scopes/grants are deployment facts not verified here. | Use attached workload identities, SDK refresh, least-privilege IAM; revoke grants and credentials at their issuer. Do not add JSON service-account keys. |
| `GMAIL_APP_PASSWORD` / Google account operator | Mailer and auth/Records/Support jobs; `GMAIL_USER` identifies the account. No expiry/refresh mechanism implemented. | Secret Manager stores/version-pins the credential; the provider must revoke/replace it. Prefer provider-supported short-lived auth where available. |
| `ZOOM_CLIENT_SECRET` / Zoom | Support provider uses `ZOOM_ACCOUNT_ID` and `ZOOM_CLIENT_ID` to obtain access tokens; provider code caches/renews those tokens. | Preserve issuer token expiry, refresh on demand, and revoke/rotate the client secret at Zoom; Secret Manager alone cannot expire it. |
| `HONOUR_JOB_SECRET` / application operator | `X-Honour-Job-Secret` middleware; shared static value, no automatic TTL/rotation. | Replace with authenticated workload invocation in foundations; coordinate callers during transition. |
| `GEMINI_KEY` / model provider | Loaded by config; current server Vertex initialization uses ADC rather than this value. | Confirm any remaining consumers before retiring it. Never infer a deployed key or necessity from a configuration field. |
| NIA caller authentication / service owner | Go NIA adapter attaches no caller key or identity token today; TLS authenticates the remote endpoint, not the calling workload. | Private Cloud Run invocation using audience-scoped Google ID tokens. Inventory/retire any separate client-facing NIA key in its own deployment. |
| NIA provider keys / respective providers | Outside this repository; values, consumers and lifetimes not verified. | NIA owner must inventory each key and its issuer's expiry/rotation capabilities before foundations are complete. |
| CI federation / Google and GitHub | Existing deployment workflow uses workload identity federation. | Retain federation, inspect trust/roles separately, and avoid adding static deployment keys. |

Secret Manager migration, IAM changes, database credential rotation, and NIA changes are not performed in step 1.

## Historical Credential Exposure Runbook

**Procedure only: no historical query, cleanup, password reset, or production change has been run.** Fixing new inserts does not clean old rows, backups, exports, or diagnostic logs.

1. Deploy and verify the logging fix through the normal release process. Limit access to affected audit exports and log sinks. Identify the affected release/time range from deployment records.
2. An authorized operator should count affected auth actions (`LOGIN`, `PASSWORD_VERIFICATION`, `RESET_PASSWORD`) and inspect only record IDs, timestamps, action names and metadata key names. Do not select, print, export, or paste credential values. Include diagnostic SQL logs in the exposure assessment.
3. Review a bounded set of row IDs. Rebuild metadata using only approved fields such as `email` and `rememberMe`; preserve the audit row, actor, action, message, communities, and creation timestamp. Use a transaction, short lock timeout, row-count check and rollback rehearsal on synthetic data. Do not delete audit events or run an unrestricted metadata rewrite.
4. Recheck key names and affected counts without reading values. Review copies in backups, exports, Cloud Logging and third-party sinks against their access controls and retention policy. An active-database cleanup does not erase those copies.
5. The operator decides password-reset notices and token/key revocation based on the exposure window and access evidence. Expired OTPs do not remove the need to assess exposed passwords. Record response actions without including credentials.

## API Inventory and Compatibility Checks

The route appendix lists registered methods, paths, auth gates, target owners and controller handlers. Handler code is the source for query/body fields, status codes and response types; existing controller tests retain those contracts. An authentication gate does not prove adequate role/ownership authorization. Community writes and lookup endpoints are currently unauthenticated; review these explicitly before extraction instead of silently changing contracts here.

Representative executable checks cover login cookie flags and lifetimes; `/me` and refresh success/401 behavior; multipart chat/NIA error translation; file listing access and uploader names; history versions; blocked replacement for pending edit/form requests; and persisted audit redaction, user search, community array filtering and aggregates. The last five PostgreSQL scenarios use synthetic records and real service/database calls; listing and audit also exercise the HTTP handler and middleware.

No gateway, new service, public route change, scheduler migration, Cloud SQL IAM connector, or Secret Manager deployment is included in this step. Those remain in the reviewed roadmap.

### Registered Routes

| Method | Path | Auth gate | Target owner | Handler source |
| --- | --- | --- | --- | --- |
| POST | `/api/user/login` | None | Identity | [Login](../internal/auth/controller.go) |
| POST | `/api/user/signup` | None | Identity | [SignUp](../internal/auth/controller.go) |
| GET | `/api/user/me` | Access cookie | Identity | [Me](../internal/auth/controller.go) |
| POST | `/api/user/logout` | None | Identity | [Logout](../internal/auth/controller.go) |
| POST | `/api/user/refresh` | Refresh cookie | Identity | [Refresh](../internal/auth/controller.go) |
| POST | `/api/user/verify-password` | Access cookie | Identity | [VerifyPassword](../internal/auth/controller.go) |
| GET | `/api/user` | Access cookie | Identity | [GetUsers](../internal/auth/controller.go) |
| POST | `/api/user/send-otp` | None | Identity | [SendOTP](../internal/auth/controller.go) |
| POST | `/api/user/reset-password` | None | Identity | [ResetPassword](../internal/auth/controller.go) |
| GET | `/api/communities` | None | Identity | [GetAllCommunities](../internal/community/controller.go) |
| POST | `/api/communities` | None | Identity | [AddCommunities](../internal/community/controller.go) |
| GET | `/api/role` | Access cookie | Identity | [GetAllRoles](../internal/role/controller.go) |
| GET | `/api/role/user` | Access cookie | Identity | [GetRolesByUserId](../internal/role/controller.go) |
| GET | `/api/file` | Access cookie | Records | [GetAllFiles](../internal/file/controller.go) |
| POST | `/api/file/upload` | Access cookie | Records | [UploadFiles](../internal/file/controller.go) |
| GET | `/api/file/data` | Access cookie | Records | [GetFileData](../internal/file/controller.go) |
| GET | `/api/file/data/normalized` | Access cookie | Records | [GetNormalizedFileData](../internal/file/controller.go) |
| POST | `/api/file/data/normalized/sync` | Access cookie | Records | [SyncNormalizedFileData](../internal/file/controller.go) |
| DELETE | `/api/file` | Access cookie | Records | [DeleteFile](../internal/file/controller.go) |
| PUT | `/api/file/reset` | Access cookie | Records | [ResetFile](../internal/file/controller.go) |
| GET | `/api/file/access` | Access cookie | Records | [GetAllAccess](../internal/file/controller.go) |
| POST | `/api/file/access` | Access cookie | Records | [CreateAccess](../internal/file/controller.go) |
| DELETE | `/api/file/access` | Access cookie | Records | [DeleteAccess](../internal/file/controller.go) |
| GET | `/api/file/history` | Access cookie | Records | [GetFileHistory](../internal/file/controller.go) |
| POST | `/api/file/replace` | Access cookie | Records | [ReplaceFile](../internal/file/controller.go) |
| POST | `/api/file/revert` | Access cookie | Records | [RevertFile](../internal/file/controller.go) |
| POST | `/api/file/edit/request` | Access cookie | Records | [CreateEditRequest](../internal/file/controller.go) |
| POST | `/api/file/achiever-stories/request` | Access cookie | Records | [CreateAchieverStoryRequest](../internal/file/controller.go) |
| GET | `/api/file/edit/request` | Access cookie | Records | [GetEditRequests](../internal/file/controller.go) |
| GET | `/api/file/edit/photos/:requestId` | Access cookie | Records | [GetPhotosByRequest](../internal/file/controller.go) |
| GET | `/api/file/edit/docs/:requestId` | Access cookie | Records | [GetDocsByRequest](../internal/file/controller.go) |
| GET | `/api/file/photos/:rowId` | Access cookie | Records | [GetPhotosByRow](../internal/file/controller.go) |
| GET | `/api/file/docs/:rowId` | Access cookie | Records | [GetDocsByRow](../internal/file/controller.go) |
| GET | `/api/file/achiever-stories/:rowId` | Access cookie | Records | [GetAchieverStoriesByRow](../internal/file/controller.go) |
| GET | `/api/file/photo/:photoId` | Access cookie | Records | [GetPhoto](../internal/file/controller.go) |
| GET | `/api/file/doc/:docId` | Access cookie | Records | [GetDoc](../internal/file/controller.go) |
| GET | `/api/file/achiever-stories/download/:storyId` | Access cookie | Records | [DownloadAchieverStory](../internal/file/controller.go) |
| GET | `/api/file/achiever-stories/request/download/:storyId` | Access cookie | Records | [DownloadRequestedAchieverStory](../internal/file/controller.go) |
| PUT | `/api/file/approve/request` | Access cookie | Records | [ReviewEditRequest](../internal/file/controller.go) |
| POST | `/api/file/photos/review` | Access cookie | Records | [ReviewPhotos](../internal/file/controller.go) |
| POST | `/api/file/doc/download/:id` | Access cookie | Records | [DownloadMediaByID](../internal/file/controller.go) |
| GET | `/api/form` | Access cookie | Records | [GetFormsByFileID](../internal/formsubmission/controller.go) |
| GET | `/api/form/answers` | Access cookie | Records | [GetFormSubmission](../internal/formsubmission/controller.go) |
| GET | `/api/form/answers/active` | Access cookie | Records | [GetActiveFormSubmission](../internal/formsubmission/controller.go) |
| POST | `/api/form/answers` | Access cookie | Records | [SaveFormSubmission](../internal/formsubmission/controller.go) |
| POST | `/api/form/answers/review` | Access cookie | Records | [ReviewFormSubmission](../internal/formsubmission/controller.go) |
| GET | `/api/form/answers/upload/:id` | Access cookie | Records | [GetUpload](../internal/formsubmission/controller.go) |
| POST | `/api/form/search` | Access cookie | Records | [SearchFormSubmissions](../internal/formsubmission/controller.go) |
| POST | `/api/form/my-requests` | Access cookie | Records | [SearchMyFormSubmissions](../internal/formsubmission/controller.go) |
| POST | `/api/admin` | Access cookie | Records | [SearchFileEditRequests](../internal/admin/controller.go) |
| POST | `/api/admin/download` | Access cookie | Records | [DownloadUpdates](../internal/admin/controller.go) |
| POST | `/api/admin/details` | Access cookie | Records | [GetFileEditRequestDetails](../internal/admin/controller.go) |
| POST | `/api/admin/download_files` | Access cookie | Records | [DownloadMediaZip](../internal/admin/controller.go) |
| GET | `/api/admin/details/:request_id` | Access cookie | Records | [GetFileEditRequestDetailsByParam](../internal/admin/controller.go) |
| GET | `/api/config` | Access cookie | Records | [GetConfig](../internal/dataconfig/controller.go) |
| GET | `/api/lookup/province` | None | Records | [GetAllProvinces](../internal/lookup/controller.go) |
| GET | `/api/lookup/dayschool/:province` | None | Records | [GetDaySchoolsByProvince](../internal/lookup/controller.go) |
| GET | `/api/lookup/hospital/:province` | None | Records | [GetIndianHospitalsByProvince](../internal/lookup/controller.go) |
| GET | `/api/logo-content/:fileId` | Access cookie | Records | [GetHTMLByFileID](../internal/logocontent/controller.go) |
| GET | `/api/file/honour` | Access cookie | Records | [GetToday](../internal/honour/controller.go) |
| POST | `/api/internal/jobs/honour/run` | Job secret header | Records | [RunDaily](../internal/honour/controller.go) |
| POST | `/api/support-requests` | Access cookie | Support | [Create](../internal/supportrequest/controller.go) |
| GET | `/api/support-requests/mine` | Access cookie | Support | [ListMine](../internal/supportrequest/controller.go) |
| GET | `/api/support-requests/admin` | Access cookie | Support | [ListForAdmin](../internal/supportrequest/controller.go) |
| PUT | `/api/support-requests/:id` | Access cookie | Support | [Update](../internal/supportrequest/controller.go) |
| GET | `/api/support-schedule/settings` | Access cookie | Support | [GetSettings](../internal/supportschedule/controller.go) |
| PUT | `/api/support-schedule/settings` | Access cookie | Support | [UpdateSettings](../internal/supportschedule/controller.go) |
| GET | `/api/support-schedule/availability` | Access cookie | Support | [ListAvailability](../internal/supportschedule/controller.go) |
| GET | `/api/support-schedule/calendar` | Access cookie | Support | [ListCalendar](../internal/supportschedule/controller.go) |
| GET | `/api/support-schedule/schedule` | Access cookie | Support | [ListSchedule](../internal/supportschedule/controller.go) |
| GET | `/api/support-schedule/fairness` | Access cookie | Support | [ListFairnessStats](../internal/supportschedule/controller.go) |
| GET | `/api/support-schedule/audit-log` | Access cookie | Support | [ListAuditLog](../internal/supportschedule/controller.go) |
| GET | `/api/support-schedule/team` | Access cookie | Support | [ListSelectableStaff](../internal/supportschedule/controller.go) |
| GET | `/api/support-schedule/team/manage` | Access cookie | Support | [ListTeam](../internal/supportschedule/controller.go) |
| PUT | `/api/support-schedule/team/:userID` | Access cookie | Support | [SetTeamMember](../internal/supportschedule/controller.go) |
| GET | `/api/support-schedule/requests` | Access cookie | Support | [ListRequests](../internal/supportschedule/controller.go) |
| POST | `/api/support-schedule/requests` | Access cookie | Support | [CreateCall](../internal/supportschedule/controller.go) |
| PUT | `/api/support-schedule/requests/:id/decision` | Access cookie | Support | [DecideRequest](../internal/supportschedule/controller.go) |
| PUT | `/api/support-schedule/requests/:id/accept-alternative` | Access cookie | Support | [AcceptAlternative](../internal/supportschedule/controller.go) |
| PUT | `/api/support-schedule/requests/:id/cancel` | Access cookie | Support | [CancelRequest](../internal/supportschedule/controller.go) |
| GET | `/api/support-schedule/calls` | Access cookie | Support | [ListCalls](../internal/supportschedule/controller.go) |
| POST | `/api/support-schedule/calls/:id/zoom/start` | Access cookie | Support | [StartZoomMeeting](../internal/supportschedule/controller.go) |
| PUT | `/api/support-schedule/calls/:id/complete` | Access cookie | Support | [CompleteCall](../internal/supportschedule/controller.go) |
| PUT | `/api/support-schedule/calls/:id/reassign` | Access cookie | Support | [ReassignCall](../internal/supportschedule/controller.go) |
| PUT | `/api/support-schedule/schedule/:date/reassign` | Access cookie | Support | [ReassignDay](../internal/supportschedule/controller.go) |
| GET | `/api/support-schedule/profile` | Access cookie | Support | [GetProfile](../internal/supportschedule/controller.go) |
| GET | `/api/support-schedule/profile/availability` | Access cookie | Support | [ListUnavailability](../internal/supportschedule/controller.go) |
| POST | `/api/support-schedule/profile/availability` | Access cookie | Support | [CreateUnavailability](../internal/supportschedule/controller.go) |
| PUT | `/api/support-schedule/profile/availability/:id` | Access cookie | Support | [UpdateAvailability](../internal/supportschedule/controller.go) |
| DELETE | `/api/support-schedule/profile/availability/:id` | Access cookie | Support | [DeleteAvailability](../internal/supportschedule/controller.go) |
| POST | `/api/support-schedule/maintenance` | Access cookie | Support | [RunMaintenance](../internal/supportschedule/controller.go) |
| POST | `/api/logs` | Access cookie | Audit | [GetLogs](../internal/logs/controller.go) |
| POST | `/api/chat` | Access cookie | Chat API | [Chat](../internal/chat/controller.go) |
| GET | `/api/chat/history` | Access cookie | Chat API | [History](../internal/chat/controller.go) |
| POST | `/api/chat/tts` | Access cookie | Chat API | [TTS](../internal/chat/controller.go) |
| GET | `/api/chat/describe/:id` | Access cookie | Chat API | [Describe](../internal/chat/controller.go) |
