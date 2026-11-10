# Milestone 3 local verification

Verified on 10 November 2026 against the working source. These are local results, not deployed release evidence. The release source is still uncommitted.

| Check | Result |
| --- | --- |
| Full Go suite, `go test -tags=integration ./... -count=1 -coverpkg=./...` | Passed against disposable PostgreSQL 15. |
| Migration `003`, rerun, existing-account compatibility, grants | Passed in a separate generated local database. Notifications cannot read business/auth tables; worker cannot read signup challenges. |
| Concurrent PostgreSQL task claims and final signup | One task acquires the provider attempt; one account and welcome intent consume the proof. |
| Outbox/business rollback, queue outage recovery, duplicate commands/tasks, uncertain acceptance, retry cap, operator replay | Passed. |
| Support/Zoom outbox | No confirmation intent before a persisted participant URL; two recipient intents after recovery; no direct SMTP or duplicate recovery intent; expired snapshots erased. |
| File/form review recovery | Queued/unresolved old rows cannot starve new intent batches; only provider acceptance sets flags, including pending-status updates emitted by an actual review. Targeted jobs/file/form regression and static checks passed after this final correction. |
| Signup HTTP and backend preflights | Request/verify create no user or login cookie; final signup is idempotent; mandatory proof rejects direct signup; `Idempotency-Key` allowed in CORS. |
| New platform coverage gate | Notifications **91.2%**, gateway **98.7%**, worker core **96.2%**. Existing process-entrypoint exclusions remain explicit in the checker. |
| Race checks | Passed for gateway, circuit, jobs, database, requestscope, mailer, workload, observability, chat, auth, notifications, notificationcontract, and outbox. |
| `go vet ./...` | Passed. |
| Real Envoy routing/streaming/failure regression | Passed with the pinned Envoy image and disposable local PostgreSQL. |
| Terraform formatting, validation and mocks | Passed: safety **8**, small database **14**, rollout **16**, Notifications **5**; **43** mock tests total. No real cloud apply. |
| Workflow lint | API workflows and companion NIA workflow passed actionlint. |
| Python tests | Existing Milestone 2 script tests **13**; service-selection tests **10** passed. |
| Actual Linux dependency graph selection | Notifications source selects Notifications; worker Dockerfile selects worker; auth selects legacy/worker; workload token shared code selects all four; documentation selects none. |
| Notifications Dockerfile | Built successfully; runtime UID **10001**, executable and writable `/app` verified. Remaining service executables also compiled with `CGO_ENABLED=0`. No image pushed. |
| Companion frontend | **13 tests / 2 suites passed** for signup and its verification hook. Production build succeeded with the existing application lint warnings. Strict `CI=true` build treats those existing warnings as errors; no unrelated lint cleanup was included. |

Still required for deployment: reviewed commits and CI artifacts; fresh live capacity/IAM inventory; reviewed Terraform plan; key versions and additive migration/grants; real Cloud Run/Tasks signed-token and caller-role validation; controlled Gmail/screenshot delivery and recovery; cold-start acceptance; frontend rollout; mandatory signup-proof activation; existing functionality acceptance. See [the release guide](milestone-3-release.md).
