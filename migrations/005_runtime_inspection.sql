-- Install before deploying the Worker inspection endpoint. Only the existing
-- Worker role receives EXECUTE; it never receives pg_read_all_stats membership.
\set ON_ERROR_STOP on
BEGIN;
SET LOCAL lock_timeout = '2s';
SET LOCAL statement_timeout = '30s';
CREATE OR REPLACE FUNCTION public.runtime_connection_counts(owners jsonb)
RETURNS TABLE(application_name text, state text, owner text, connection_count bigint)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
 -- Fail closed if the installation identity cannot inspect other sessions.
 IF NOT pg_has_role(current_user,'pg_read_all_stats','USAGE')
    AND NOT (SELECT rolsuper FROM pg_catalog.pg_roles WHERE rolname=current_user) THEN
  RAISE EXCEPTION 'connection inspection owner lacks statistics visibility';
 END IF;
 RETURN QUERY
 SELECT a.application_name,a.state,COALESCE(owners ->> a.usename,'external'),count(*)
 FROM pg_catalog.pg_stat_activity a
 WHERE a.datname=current_database() AND a.backend_type='client backend'
 GROUP BY a.application_name,a.state,COALESCE(owners ->> a.usename,'external')
 ORDER BY a.application_name,a.state,COALESCE(owners ->> a.usename,'external');
END $$;
REVOKE ALL ON FUNCTION public.runtime_connection_counts(jsonb) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.runtime_connection_counts(jsonb) TO :"worker_user";
COMMIT;
