# Milestone 3 verification

Local checks and the development rollout were performed on 10 November 2026. The table below records local checks; the development evidence that follows distinguishes actual cloud results from local fault-injection tests.

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
| Terraform formatting, validation and mocks | Passed: safety **8**, small database **14**, rollout **16**, Notifications **5**; **43** mock tests total. The mock run did not apply cloud resources; reviewed development applies are recorded below. |
| Workflow lint | API workflows and companion NIA workflow passed actionlint. |
| Python tests | Existing Milestone 2 script tests **13**; service-selection tests **10** passed. |
| Actual Linux dependency graph selection | Notifications source selects Notifications; worker Dockerfile selects worker; auth selects legacy/worker; workload token shared code selects all four; documentation selects none. |
| Notifications Dockerfile | Built successfully; runtime UID **10001**, executable and writable `/app` verified. Remaining service executables also compiled with `CGO_ENABLED=0`. The local build did not publish an image; reviewed release images were subsequently published by GitHub Actions. |
| Companion frontend | **13 tests / 2 suites passed** for signup and its verification hook. Production build succeeded with the existing application lint warnings. Strict `CI=true` build treats those existing warnings as errors; no unrelated lint cleanup was included. |

## Development rollout evidence

The rollout targets only `planar-ray-472112-e8`, `us-west1`, and database `nordikdrive` / `postgres`.

| Check | Recorded result |
| --- | --- |
| Reviewed API release | Application source from PR #135; workflow repair in PR #136, master `f1f7a45d2341e96a98889add4fc1bc38238d37ae`. [Release run 42881035761](https://github.com/databasenordik/nordikdriveapi/actions/runs/42881035761) passed. |
| Frontend | Initial PR #78 deployed; two-step correction [PR #79](https://github.com/databasenordik/nordik-drive/pull/79) approved and merged as `8e9ed072a1b7e8f8e16c0508fd163a921c8f9ada`. Required unit and Cypress checks passed on final head `f5caa3137efbb100f203b3170b20b4e1fafe3082`. |
| Database safety | Fresh maximum 25, reserved 3. Configured allocation remains 19 of the 20-connection safeguard, including replacement overlap and external consumers. Observed client connections were 6 to 8 during release checks. |
| Backup | Backup `1794338398901`, description `Milestone-3-before-notifications-schema`, completed successfully before migration. |
| Additive migration/grants | Migration 003 and grants applied. All 49 existing users remained unchanged; none were silently marked verified. Notifications cannot read users/outboxes; worker cannot read signup challenge state. |
| Secrets | New payload and signup key-map secrets use numeric Secret Manager version 1 and logical version 1. Existing JWT, SMTP and Zoom references remain version 1. No key rotation was performed. |
| Controlled OTP delivery | Actual legacy caller to private Notifications to Cloud Tasks task identity to Gmail provider acceptance succeeded on first attempt. Initial delivery took about 1.5 seconds; the user confirmed inbox receipt. |
| Private OTP checks | Wrong code rejected; correct verification succeeded; duplicate request/verification reused the challenge/proof; one email delivery; no account or login cookie created. |
| Public API / CORS | OTP request and retry passed through the unchanged public API URL. Browser preflights allow `Idempotency-Key`; request created no login cookie. |
| Worker rollout | Schedules paused and running jobs drained before transport cutover. New worker promoted before schedules resumed. Dispatcher, review-email, normalization and reconciliation runs succeeded. Old `honour-daily` remains paused. |
| Signup enforcement and panic repair | [Backend PR #137](https://github.com/databasenordik/nordikdriveapi/pull/137) deployed as `5571dccdbb4e930e244eb0e8c682c1bd5df69254`. The typed-nil SMTP client no longer panics during request scoping. Public signup without proof returns 400; real verified signup returned 201 and login returned 200. |
| Private endpoint protection | Unauthenticated Notifications and worker calls return 403; the public gateway does not expose the internal notification endpoint. |
| Rendered UI regression | Production build in isolated Chromium proved enabled white-on-navy Sign up, required-field validation, separate OTP screen, no creation before verification, preserved details, and reuse of an unexpired challenge. Final outlined-action checks also run in required Cypress CI. |
| Failure/replay behavior | Retry ceilings, unknown SMTP acceptance, audited operator replay, concurrent claims, rollback, expiry, and immutable screenshot validation passed local real-PostgreSQL tests. These are not claims that SMTP faults were injected into the live development service. |

## Final deployed release and acceptance

[Frontend deployment](https://github.com/databasenordik/nordik-drive/actions/runs/42894969624) and [backend release](https://github.com/databasenordik/nordikdriveapi/actions/runs/42900787873) succeeded. Required frontend and backend CI passed. The shared mailer fix selected legacy, worker, and Notifications through the dependency graph.

| Runtime | Serving revision |
| --- | --- |
| Frontend | `nordik-drive-react-00111-h6f` |
| Gateway | `nordikdriveapi-00341-2xq` |
| Legacy | `nordikdriveapi-legacy-00009-wjk` |
| Worker | `nordikdriveapi-worker-00008-f5f` |
| Notifications | `nordikdriveapi-notifications-00004-6tf` |

The real browser check passed details validation, the separate OTP screen, readable primary/outlined buttons, code verification, exactly one verified account, signup HTTP 201, login HTTP 200, and navigation to the files page. The OTP was provider-accepted on its first attempt. The welcome email was provider-accepted once, about 27.8 seconds after its intent was created. The associated database check found no failed or unknown deliveries and eight client connections. These measurements were taken during the 10 November rollout.

Development deployment is complete. Full acceptance remains open for:

- Existing sessions, actual file downloads, chat, and Support/Zoom regression after the final fix. Earlier Milestone 2 acceptance does not count as Milestone 3 confirmation.
- A measured Notifications startup and delivery from zero instances. Deployment startup probes and warm delivery checks do not establish this result.

Status reviewed on 11 November 2026. Production remains outside this rollout.

## Completion update prepared on 11 November

The completion source adds automatic first-processing tasks, isolated bounded repair phases, and finite signup expiry cleanup. Normalization is configured for six hours; other repair/cleanup scans for twelve hours. Feature defaults retain the existing scans until the private source and immediate task paths pass cloud acceptance. See [the completion rollout](milestone-3-completion.md).

Local checks passed: full Go regression, real PostgreSQL integration (including cleanup retention, locked rows, singleton lock, one-connection operation, and migration 004), static checks, 51 Terraform mock runs, 13 inventory/migration/secret-script tests, and 10 service-selection tests. Combined coverage: gateway 98.7%, Notifications 91.9%, Worker core 96.2%. Focused tests also prove that an unrelated Support backlog does not delay a new finalized notification.

This update has not been deployed. The serving development revisions above were rechecked unchanged. Envoy/race results, the reviewed release commit, new digests/revisions, final schedules, cold delivery, and browser acceptance must be recorded when completed. Existing JWT/key values, database tier, and production remain unchanged.
