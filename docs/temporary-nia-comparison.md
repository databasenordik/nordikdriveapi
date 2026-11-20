# Temporary development NIA comparison — November 17, 2026

The authenticated chat UI offers **NIA API** and **API-based NIA** when the development Legacy service has `CHAT_COMPARISON_UNTIL=2026-11-18T05:00:00Z`. Do not enable the selector on production. This window ends at midnight after November 17 in Toronto; the Go server rejects a later configured deadline. With no value, the default engine and response contract remain unchanged. This setting controls only engine selection, not logging. NIA no longer needs this setting.

The UI discovers availability through authenticated `GET /api/chat/comparison`. Comparison questions use the existing `POST /api/chat` with multipart `engine=nia` or `engine=api`. Default NIA history stays on its current server-backed thread. The API-based engine has a separate, browser-session thread; switching back preserves each engine's messages and never reuses the other engine's conversation ID. Switching is disabled during a pending answer. The UI hides the selector at expiry; the server also rejects expired overrides.

## Timing and logs

Both implementations permanently emit `event=nia_comparison`, `engine`, `request_id`, `question`, `answer`, `failed`, and `timing`. Logging and provider capture have no date-based expiry and do not require `CHAT_COMPARISON_UNTIL`. Timing contains `measurement=provider_round_trip`, `provider_calls` (model, duration_ms, outcome), `llm_ms` (sum), and `llm_call_count`. NIA always returns timing in the existing internal gRPC result JSON so Legacy can log the actual remote provider measurements. The Legacy public response includes timing only during the engine-comparison window. Missing remote measurements are explicitly unavailable, with null totals; they are never substituted with zero.

Measure immediately before sending the prepared provider request and finish after its full response is consumed. Exclude database reads, retrieval, prompt construction, tools, admission queues, retry backoff, frontend/API/gRPC transport, and NIA audio transcription. Include network travel between the backend and model provider: these client measurements cannot isolate provider compute time. Each fallback/HTTP attempt is reported separately. A cached, shared, or deterministic answer may originate no new provider calls; zero calls is not a fast LLM measurement. Compare samples with calls, the same question, file/community selection, and equivalent conversation context. Models and reasoning settings are deliberately unchanged and appear in the samples.

NIA's older whole-engine duration is called `engine_ms`; it must not be treated as provider-only time. The standalone NIA gRPC logger emits the source measurement; Legacy also emits the returned NIA summary under the same request ID. For one record per question across both engines, filter the **Legacy service**:

```text
resource.type="cloud_run_revision"
resource.labels.service_name="nordikdriveapi-legacy"
jsonPayload.event="nia_comparison"
```

To inspect the original NIA measurement, use service `nordik-nia-api` and the same request ID. Question/answer and provider timing logs continue after the selector expires. No retrieved records, system prompts, cookies, credentials, or full provider request bodies are added to these logs.

## Release and removal

Review and merge the API, NIA and frontend changes through their existing checks and approval rules. Publish images independently. Deploy only the existing development services, preserving private caller rules, database pools, JWT/secrets, runtime identities and the established serialized Legacy rollout procedure. No database migration or new service is required. The NIA deployment workflow must preserve existing IAM and keep its revision cap at one, matching the existing service cap.

Validate each engine from the same frontend: submit a controlled question, confirm the selected backend answers, check matching request IDs/model attempts in logs, switch back and confirm histories remain separate. Confirm the selector cannot change during a pending answer. At expiry, default NIA continues and the selector disappears.

After the comparison, the expired `CHAT_COMPARISON_UNTIL` settings can be removed through normal review/deployment. Any later removal of the selector/capability endpoint and engine override must retain provider capture, internal timing propagation, and permanent question/answer logs. Automatic selector expiry does not delete source code or retained Cloud Logging entries. No scheduled job or always-running instance is introduced.
