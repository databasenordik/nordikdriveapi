# NIA chat integration

`POST /api/chat` keeps its existing form fields (`filename`, `question`, optional
`communities` and `audio`) and `answer` response. `CHAT_USE_NIA=false` (default)
uses the existing Go chat implementation. `CHAT_USE_NIA=true` selects the NIA
adapter at process startup. `/api/chat/describe/:id` continues to use Go.

Set these Cloud Run environment variables after the NIA gRPC service is ready:

```text
CHAT_USE_NIA=true
NIA_GRPC_TARGET=nordik-nia-api-724838782318.us-west1.run.app:443
NIA_GRPC_TLS=true
NIA_GRPC_TIMEOUT_SECONDS=300
```

The target is a host and port, without `https://`. TLS verifies Cloud Run's server
certificate; no service identity token or client certificate is sent. For local
testing use `NIA_GRPC_TARGET=127.0.0.1:8080` and `NIA_GRPC_TLS=false`.

The adapter resolves the filename to its database file ID and forwards the
question and community selections using `proto/nia.proto`. It reuses one gRPC
connection, propagates request cancellation, and reports NIA failures with HTTP
errors. It does not switch back to Go automatically on failure. Audio questions
are transcribed through the existing Gemini client before forwarding the text.

NIA must be deployed with `NIA_TRANSPORT=grpc`, a configured
`NIA_GRPC_PRINCIPAL_ID`, intended `NIA_GRPC_ALLOWED_FILE_IDS`, its database and
reasoning settings, and migration `069_grpc_scoped_retrieval.sql`. Enable HTTP/2
and a TCP startup probe on that Cloud Run service. This initial gRPC endpoint
does not require authentication; all calls use the configured NIA data scope.
The NIA repository's `docs/GRPC_INTEGRATION.md` contains the full setup.

An optional `conversation_id` form field supports unfiltered NIA follow-ups.
Community-filtered follow-ups are currently rejected. `matched_row_id` is omitted
for NIA answers because NIA does not supply an equivalent exact row reference.

Run `go test ./internal/chat/... ./cmd/server`. To additionally test against the
running Python NIA fixture service, set `NIA_TEST_TARGET=127.0.0.1:8080` and run
`go test ./internal/chat -run TestNIAChatController_PythonIntegration -v`.
