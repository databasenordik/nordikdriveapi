-- Run only within the coordinated Identity relocation window after old auth,
-- notification dispatch and cleanup executions have drained. The release runner
-- records the exact tested compatible revisions before supplying this gate.
\set ON_ERROR_STOP on
\if :compatibility_verified
\else
 \echo 'Identity compatibility gate has not passed'
 \quit 3
\endif
BEGIN;
SET LOCAL lock_timeout = '2s';
SET LOCAL statement_timeout = '30s';
-- Existing FKs, views and owned sequences follow the physical relation OIDs.
-- Neither missing user_roles nor missing access tables are created here.
DO $$ DECLARE name text; BEGIN
 FOREACH name IN ARRAY ARRAY['users','roles','communities','otps','signup_otp_challenges','signup_otp_limits','signup_idempotency'] LOOP
  IF to_regclass('public.'||name) IS NOT NULL AND to_regclass('identity.'||name) IS NOT NULL THEN
   RAISE EXCEPTION 'Ambiguous Identity table %',name;
  ELSIF to_regclass('public.'||name) IS NOT NULL THEN
   EXECUTE format('ALTER TABLE public.%I SET SCHEMA identity',name);
  ELSIF to_regclass('identity.'||name) IS NULL THEN
   RAISE EXCEPTION 'Required Identity table % is absent',name;
  END IF;
 END LOOP;
END $$;
-- A single transaction moves complete encrypted commands and all delivery state.
-- A conflict is an error; do not overwrite or regenerate an existing command.
WITH moved AS (DELETE FROM public.notification_outbox WHERE producer='identity' RETURNING *)
 INSERT INTO identity.notification_outbox SELECT * FROM moved;
GRANT USAGE ON SCHEMA identity TO :"legacy_user",:"worker_user";
-- Temporary compatibility only. Remove in 008 after rollback compatibility proof.
GRANT SELECT ON identity.users TO :"legacy_user",:"worker_user";
COMMIT;
