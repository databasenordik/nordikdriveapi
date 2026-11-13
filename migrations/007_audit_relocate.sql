\set ON_ERROR_STOP on
\if :compatibility_verified
\else
 \echo 'Audit compatibility gate has not passed'
 \quit 3
\endif
BEGIN;
SET LOCAL lock_timeout = '2s';
SET LOCAL statement_timeout = '30s';
DO $$ BEGIN
 IF to_regclass('public.logs') IS NOT NULL AND to_regclass('audit.logs') IS NOT NULL THEN
  RAISE EXCEPTION 'Ambiguous Audit activity table';
 ELSIF to_regclass('public.logs') IS NOT NULL THEN
  ALTER TABLE public.logs SET SCHEMA audit;
 ELSIF to_regclass('audit.logs') IS NULL THEN
  RAISE EXCEPTION 'Historical activity table is absent';
 END IF;
END $$;
COMMIT;
