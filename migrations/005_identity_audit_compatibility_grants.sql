-- Restricted IAM database usernames are psql identifiers, never passwords.
-- Compatibility grants precede relocation. No grant to all public tables.
\set ON_ERROR_STOP on
BEGIN;
GRANT USAGE ON SCHEMA public,identity TO :"identity_user";
GRANT SELECT,INSERT,UPDATE,DELETE ON public.users,public.roles,public.communities,public.otps,
 public.signup_otp_challenges,public.signup_otp_limits,public.signup_idempotency TO :"identity_user";
GRANT SELECT,INSERT,UPDATE ON identity.notification_outbox,identity.audit_outbox TO :"identity_user";
GRANT DELETE ON identity.notification_outbox TO :"identity_user";
-- Identity initially preserves activity writes until the Audit producer gate.
GRANT INSERT ON public.logs TO :"identity_user";
-- The existing GORM logger uses INSERT ... RETURNING id. Grant only that
-- generated column; activity messages and metadata remain unreadable.
GRANT SELECT (id) ON public.logs TO :"identity_user";
SELECT format('GRANT USAGE,SELECT ON SEQUENCE %s TO %I', seq, :'identity_user')
 FROM (SELECT pg_get_serial_sequence('public.'||name,'id') AS seq FROM unnest(ARRAY['users','roles','communities','otps','logs']) name) s
 WHERE seq IS NOT NULL \gexec
GRANT SELECT,INSERT,UPDATE ON public.audit_outbox TO :"legacy_user";
COMMIT;
