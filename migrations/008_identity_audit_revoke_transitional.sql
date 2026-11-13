-- This gate is separate from physical relocation. A rollback must use an exact
-- tested compatible image and owner APIs, rather than restoring broad grants.
\set ON_ERROR_STOP on
\if :compatible_rollback_verified
\else
 \echo 'Compatible rollback gate has not passed'
 \quit 3
\endif
BEGIN;
REVOKE ALL ON identity.users,identity.roles,identity.communities,identity.otps,
 identity.signup_otp_challenges,identity.signup_otp_limits,identity.signup_idempotency
 FROM :"legacy_user",:"worker_user";
-- Physical foreign-key checks and the installed NIA view remain dependencies.
-- Application SELECT access to Identity tables is not needed for FK enforcement.
REVOKE ALL ON audit.logs FROM :"legacy_user",:"worker_user",:"identity_user";
SELECT format('REVOKE ALL ON SEQUENCE %s FROM %I,%I,%I',seq,:'legacy_user',:'worker_user',:'identity_user')
 FROM (SELECT pg_get_serial_sequence('audit.logs','id') seq) s WHERE seq IS NOT NULL \gexec
SELECT format('REVOKE ALL ON SEQUENCE %s FROM %I,%I',seq,:'legacy_user',:'worker_user')
 FROM (SELECT pg_get_serial_sequence('identity.'||name,'id') seq FROM unnest(ARRAY['users','roles','communities','otps']) name) s
 WHERE seq IS NOT NULL \gexec
-- Worker dispatches privately to Identity; it receives no Identity outbox grant.
REVOKE ALL ON identity.notification_outbox,identity.audit_outbox FROM :"legacy_user",:"worker_user";
COMMIT;
