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
`NIA_GRPC_PRINCIPAL_ID`, its database and reasoning settings, and migration
`069_grpc_scoped_retrieval.sql`. Each request is scoped to the file selected by Go;
there is no fixed `NIA_GRPC_ALLOWED_FILE_IDS` setting. Enable HTTP/2
and a TCP startup probe on that Cloud Run service. This initial gRPC endpoint
does not require authentication; all calls use the configured NIA data scope.
The NIA repository's `docs/GRPC_INTEGRATION.md` contains the full setup.

Chat requests forward the authenticated user ID as `QueryRequest.user_id` and the
resolved file ID as `selected_file_id`. User identity comes from the existing
`access_token` cookie, never from a form field. NIA keeps its configured data
principal for dataset grants and separately checks the application's user ID.

NIA owns the single conversation ID for each user/file pair. It stores `user_id`
and `file_id` on the existing `assistant.conversations` table (`file_id` is a stored
generated alias of NIA's existing `selected_file_id` binding). Messages
stay in `assistant.messages`; there are no Go chat-history tables or separate
public/upstream IDs. Omitting `conversation_id` resumes the same user/file thread.
An explicit ID must belong to that user and file or the request returns 404.

Run the incremental `init.sql` in this repository against the existing NIA
database before deploying both updated services. It contains only the new changes
from `074_user_file_conversations.sql` and records that migration as applied. `assistant.sql` in this repository bundles the NIA SQL, excluding
standalone application stubs and demo-only grants. It requires the existing
migrator privileges and role-password settings described at the top of the file.
Legacy conversations without a trustworthy user mapping keep `user_id = NULL`
and are not assigned to an application user automatically.

### UI history

Call `GET /api/chat/history?filename=students.xlsx` to fetch the latest 10 complete
question/answer pairs, returned in chronological order. Set `limit=1` through
`limit=100` to change that number; an optional `conversation_id` must match the
user and file. Go calls NIA's `History` gRPC method, which reads persisted messages
rather than the assistant's short model-context snapshot.

```json
{
  "conversation_id": "nia-conversation-uuid",
  "file_id": 49,
  "entries": [
    {"id": "exchange-uuid", "exchange_id": "exchange-uuid", "question_id": "question-message-uuid", "answer_id": "answer-message-uuid", "question": "How many students?", "answer": "...", "created_at": "2026-10-15T15:00:00Z", "communities": []}
  ],
  "has_more": false
}
```

No history returns a null conversation ID and an empty entries array. `has_more`
indicates older pairs exist; the endpoint does not paginate older entries.
History requires the NIA backend; the legacy Go backend returns 503 for history.
Community-filtered questions use the same user/file conversation and persist
their community selection on each message. Changing filters resets model context
without deleting saved history, so earlier results from another filter are not reused. `matched_row_id` is omitted for NIA answers.

Run `go test ./internal/chat/... ./cmd/server`. To additionally test against the
running Python NIA fixture service, set `NIA_TEST_TARGET=127.0.0.1:8080` and run
`go test ./internal/chat -run TestNIAChatController_PythonIntegration -v`.

Each question/answer pair shares an `exchange_id`; each message keeps its own
`assistant.messages.id`. POST replies include `conversation_id` and `exchange_id`.
The UI loads the last 10 persisted pairs on opening a file chat, retains that
conversation ID for follow-ups, and clears cached chat state on account changes.

Upgrade an already initialized NIA database (PowerShell):

```powershell
psql "$env:ASSISTANT_MIGRATOR_DATABASE_URL" -v ON_ERROR_STOP=1 -f init.sql
```

Do not run the full `assistant.sql` for this incremental upgrade. No production
database changes are performed by the application code update itself.
