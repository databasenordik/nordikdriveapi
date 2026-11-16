# Temporary development NIA comparison — November 17, 2026

The authenticated chat UI offers **NIA API** and **API-based NIA** when the development Legacy service has `CHAT_COMPARISON_UNTIL=2026-11-18T05:00:00Z`. Set the same value on the existing development NIA service. Do not enable it on production. This window ends at midnight after November 17 in Toronto; both servers reject a later configured deadline. With no value, the default engine and response contract remain unchanged.

The UI discovers availability through authenticated `GET /api/chat/comparison`. Comparison questions use the existing `POST /api/chat` with multipart `engine=nia` or `engine=api`. Default NIA history stays on its current server-backed thread. The API-based engine has a separate, browser-session thread; switching back preserves each engine's messages and never reuses the other engine's conversation ID. Switching is disabled during a pending answer. The UI hides the selector at expiry; the server also rejects expired overrides.

## Timing and logs

Both implementations emit `event=nia_comparison`, `engine`, `request_id`, `question`, `answer`, `failed`, and `timing`. Timing contains `measurement=provider_round_trip`, `provider_calls` (model, duration_ms, outcome), `llm_ms` (sum), and `llm_call_count`. The Legacy response also includes the timing during the comparison window. Missing remote measurements are explicitly unavailable, with null totals; they are never substituted with zero.

Measure immediately before sending the prepared provider request and finish after its full response is consumed. Exclude database reads, retrieval, prompt construction, tools, admission queues, retry backoff, frontend/API/gRPC transport, and NIA audio transcription. Include network travel between the backend and model provider: these client measurements cannot isolate provider compute time. Each fallback/HTTP attempt is reported separately. A cached, shared, or deterministic answer may originate no new provider calls; zero calls is not a fast LLM measurement. Compare samples with calls, the same question, file/community selection, and equivalent conversation context. Models and reasoning settings are deliberately unchanged and appear in the samples.

NIA's older whole-engine duration is called `engine_ms`; it must not be treated as provider-only time. The standalone NIA gRPC logger emits the source measurement; Legacy also emits the returned NIA summary under the same request ID. For one record per question across both engines, filter the **Legacy service**:

```text
resource.type="cloud_run_revision"
resource.labels.service_name="nordikdriveapi-legacy"
jsonPayload.event="nia_comparison"
```

To inspect the original NIA measurement, use service `nordik-nia-api` and the same request ID. Question/answer logging is explicitly limited to this comparison window. No retrieved records, system prompts, cookies, credentials, or full provider request bodies are added to these logs.

## Release and removal

Review and merge the API, NIA and frontend changes through their existing checks and approval rules. Publish images independently. Deploy only the existing development services, preserving private caller rules, database pools, JWT/secrets, runtime identities and the established serialized Legacy rollout procedure. No database migration or new service is required. The NIA deployment workflow must preserve existing IAM and keep its revision cap at one, matching the existing service cap.

Validate each engine from the same frontend: submit a controlled question, confirm the selected backend answers, check matching request IDs/model attempts in logs, switch back and confirm histories remain separate. Confirm the selector cannot change during a pending answer. At expiry, default NIA continues and the selector disappears.

After the comparison, remove the two `CHAT_COMPARISON_UNTIL` settings and revert the three comparison feature commits through normal review/deployment. This removes the selector/capability endpoint, engine override, provider captures and question/answer logs. Automatic expiry disables the feature but does not delete source code or retained Cloud Logging entries. No scheduled job or always-running instance is introduced.
