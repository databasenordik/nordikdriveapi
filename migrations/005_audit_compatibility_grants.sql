-- Apply when the Audit IAM database user exists, after Identity consumer gates.
\set ON_ERROR_STOP on
BEGIN;
GRANT USAGE ON SCHEMA public,audit TO :"audit_user";
GRANT SELECT,INSERT ON public.logs TO :"audit_user";
GRANT SELECT,INSERT,UPDATE ON audit.audit_ingestions TO :"audit_user";
SELECT format('GRANT USAGE,SELECT ON SEQUENCE %s TO %I', pg_get_serial_sequence('public.logs','id'), :'audit_user')
 WHERE pg_get_serial_sequence('public.logs','id') IS NOT NULL \gexec
COMMIT;
