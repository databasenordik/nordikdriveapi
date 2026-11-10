-- psql variables contain existing/new IAM database usernames, not passwords.
\set ON_ERROR_STOP on
BEGIN;
GRANT USAGE ON SCHEMA notifications TO :"notifications_user";
GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA notifications TO :"notifications_user";
GRANT USAGE,SELECT ON ALL SEQUENCES IN SCHEMA notifications TO :"notifications_user";
GRANT SELECT,INSERT,UPDATE,DELETE ON public.notification_outbox TO :"legacy_user", :"worker_user";
GRANT SELECT,INSERT,UPDATE,DELETE ON public.signup_otp_challenges,public.signup_otp_limits,public.signup_idempotency TO :"legacy_user";
COMMIT;
