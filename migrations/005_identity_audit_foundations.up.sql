-- Additive only. Existing tables and serving code remain in place at this gate.
\set ON_ERROR_STOP on
BEGIN;
SET LOCAL lock_timeout = '2s';
SET LOCAL statement_timeout = '30s';
CREATE SCHEMA IF NOT EXISTS identity;
CREATE SCHEMA IF NOT EXISTS audit;
REVOKE ALL ON SCHEMA identity,audit FROM PUBLIC;

CREATE TABLE IF NOT EXISTS identity.notification_outbox
 (LIKE public.notification_outbox INCLUDING ALL);
CREATE TABLE IF NOT EXISTS public.audit_outbox (
 id text PRIMARY KEY, fingerprint text NOT NULL, payload text NOT NULL,
 state text NOT NULL CHECK (state IN ('pending','ingested')), log_id bigint NOT NULL DEFAULT 0,
 attempts integer NOT NULL DEFAULT 0, next_attempt timestamptz NOT NULL,
 lease_until timestamptz, created_at timestamptz NOT NULL,
 CHECK (state <> 'ingested' OR (payload = '' AND log_id > 0))
);
CREATE TABLE IF NOT EXISTS identity.audit_outbox
 (LIKE public.audit_outbox INCLUDING ALL);
CREATE INDEX IF NOT EXISTS audit_outbox_due ON public.audit_outbox(next_attempt,created_at,id) WHERE state='pending';
CREATE INDEX IF NOT EXISTS audit_outbox_due ON identity.audit_outbox(next_attempt,created_at,id) WHERE state='pending';
CREATE TABLE IF NOT EXISTS audit.audit_ingestions (
 event_id text PRIMARY KEY, fingerprint text NOT NULL, producer text NOT NULL CHECK(producer IN ('identity','legacy')),
 version integer NOT NULL CHECK(version=1), request_id text NOT NULL DEFAULT '', trace_id text NOT NULL DEFAULT '',
 log_id bigint NOT NULL DEFAULT 0, created_at timestamptz NOT NULL
);
-- Reconciliation changes delivery state, never the original event or its digest.
CREATE OR REPLACE FUNCTION audit.guard_producer_event() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.id<>OLD.id OR NEW.fingerprint<>OLD.fingerprint OR NEW.created_at<>OLD.created_at
    OR (NEW.payload<>OLD.payload AND NOT (OLD.state='pending' AND NEW.state='ingested' AND NEW.payload='' AND NEW.log_id>0))
    OR (OLD.state='ingested' AND (NEW.state<>OLD.state OR NEW.log_id<>OLD.log_id)) THEN
  RAISE EXCEPTION 'audit producer event is immutable';
 END IF;
 RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS audit_event_immutable ON public.audit_outbox;
CREATE TRIGGER audit_event_immutable BEFORE UPDATE ON public.audit_outbox FOR EACH ROW EXECUTE FUNCTION audit.guard_producer_event();
DROP TRIGGER IF EXISTS audit_event_immutable ON identity.audit_outbox;
CREATE TRIGGER audit_event_immutable BEFORE UPDATE ON identity.audit_outbox FOR EACH ROW EXECUTE FUNCTION audit.guard_producer_event();
REVOKE ALL ON identity.notification_outbox,identity.audit_outbox,public.audit_outbox,audit.audit_ingestions FROM PUBLIC;
REVOKE ALL ON FUNCTION audit.guard_producer_event() FROM PUBLIC;
COMMIT;
