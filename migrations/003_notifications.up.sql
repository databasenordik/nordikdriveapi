-- Additive migration. Execute with the migration identity, never runtime startup.
BEGIN;
SET LOCAL lock_timeout = '2s';
SET LOCAL statement_timeout = '30s';
CREATE SCHEMA IF NOT EXISTS notifications;
REVOKE ALL ON SCHEMA notifications FROM PUBLIC;
CREATE TABLE IF NOT EXISTS notifications.deliveries (
 id text PRIMARY KEY, producer text NOT NULL, event_key text NOT NULL,
 command text NOT NULL, fingerprint text NOT NULL, state text NOT NULL,
 kind text NOT NULL, deadline timestamptz NOT NULL,
 supersession text NOT NULL DEFAULT '', sequence bigint NOT NULL DEFAULT 0,
 attempts integer NOT NULL DEFAULT 0, max_attempts integer NOT NULL DEFAULT 5, dispatch_generation integer NOT NULL DEFAULT 0,
 next_attempt timestamptz NOT NULL, lease_until timestamptz,
 created_at timestamptz NOT NULL, updated_at timestamptz NOT NULL,
 UNIQUE(producer,event_key)
);
CREATE INDEX IF NOT EXISTS notification_delivery_due ON notifications.deliveries(state,next_attempt);
CREATE INDEX IF NOT EXISTS notification_delivery_supersession ON notifications.deliveries(producer,supersession,sequence);
CREATE TABLE IF NOT EXISTS notifications.attempts (
 id bigserial PRIMARY KEY, delivery_id text NOT NULL REFERENCES notifications.deliveries(id),
 number integer NOT NULL, outcome text NOT NULL, created_at timestamptz NOT NULL,
 UNIQUE(delivery_id,number)
);
CREATE TABLE IF NOT EXISTS notifications.replays (
 key text PRIMARY KEY, delivery_id text NOT NULL REFERENCES notifications.deliveries(id),
 caller text NOT NULL, reason text NOT NULL, created_at timestamptz NOT NULL
);
CREATE TABLE IF NOT EXISTS public.notification_outbox (
 kind text NOT NULL, deadline timestamptz NOT NULL, payload_erased boolean NOT NULL DEFAULT false,
 id text PRIMARY KEY, producer text NOT NULL, event_key text NOT NULL, command text NOT NULL,
 state text NOT NULL, delivery_id text NOT NULL DEFAULT '', next_attempt timestamptz NOT NULL,
 lease_until timestamptz, attempts integer NOT NULL DEFAULT 0, created_at timestamptz NOT NULL,
 UNIQUE(producer,event_key)
);
CREATE INDEX IF NOT EXISTS notification_outbox_due ON public.notification_outbox(state,next_attempt);
CREATE INDEX IF NOT EXISTS notification_outbox_payload_cleanup ON public.notification_outbox(deadline) WHERE NOT payload_erased;
ALTER TABLE public.users ADD COLUMN IF NOT EXISTS email_verified_at timestamptz;
ALTER TABLE public.file_edit_request ADD COLUMN IF NOT EXISTS review_notification_key text NOT NULL DEFAULT '';
ALTER TABLE public.form_submissions ADD COLUMN IF NOT EXISTS review_notification_key text NOT NULL DEFAULT '';
ALTER TABLE public.support_requests ADD COLUMN IF NOT EXISTS notification_version bigint NOT NULL DEFAULT 0;
ALTER TABLE public.support_schedule_audit_logs ADD COLUMN IF NOT EXISTS notification_state text NOT NULL DEFAULT 'ignored';
ALTER TABLE public.support_schedule_audit_logs ADD COLUMN IF NOT EXISTS notification_next_attempt timestamptz;
ALTER TABLE public.support_schedule_audit_logs ADD COLUMN IF NOT EXISTS notification_snapshot text NOT NULL DEFAULT '';
CREATE TABLE IF NOT EXISTS public.signup_otp_challenges (
 id text PRIMARY KEY, email text NOT NULL, verifier text NOT NULL, key_version text NOT NULL,
 attempts integer NOT NULL DEFAULT 0, state text NOT NULL,
 expires_at timestamptz NOT NULL, resend_after timestamptz NOT NULL,
 proof_verifier text NOT NULL DEFAULT '', proof_expires_at timestamptz,
 created_at timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS signup_otp_email ON public.signup_otp_challenges(email,created_at DESC);
CREATE TABLE IF NOT EXISTS public.signup_otp_limits (
 key text PRIMARY KEY, count integer NOT NULL DEFAULT 0, expires_at timestamptz NOT NULL
);
CREATE TABLE IF NOT EXISTS public.signup_idempotency (
 key text PRIMARY KEY, fingerprint text NOT NULL, response text NOT NULL DEFAULT '',
 key_version text NOT NULL, expires_at timestamptz NOT NULL
);
REVOKE ALL ON ALL TABLES IN SCHEMA notifications FROM PUBLIC;
REVOKE ALL ON public.notification_outbox,public.signup_otp_challenges,public.signup_otp_limits,public.signup_idempotency FROM PUBLIC;
COMMIT;
