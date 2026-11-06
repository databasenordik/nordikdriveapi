-- Run once with a migration identity, never at application startup.
BEGIN;
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';
CREATE SCHEMA IF NOT EXISTS gateway;
REVOKE ALL ON SCHEMA gateway FROM PUBLIC;
CREATE TABLE IF NOT EXISTS gateway.rate_limits (
    policy text NOT NULL CHECK (length(policy) BETWEEN 1 AND 64),
    subject text NOT NULL CHECK (length(subject) = 64),
    window_start timestamptz NOT NULL,
    count integer NOT NULL CHECK (count > 0),
    expires_at timestamptz NOT NULL,
    PRIMARY KEY (policy, subject, window_start)
);
CREATE INDEX IF NOT EXISTS rate_limits_expiry ON gateway.rate_limits(expires_at);
REVOKE ALL ON gateway.rate_limits FROM PUBLIC;
-- Transitional monolith ownership; move each job's ledger with its domain.
CREATE TABLE IF NOT EXISTS public.platform_job_runs (
    job_name text NOT NULL,
    scheduled_at timestamptz NOT NULL,
    status text NOT NULL CHECK (status IN ('running','succeeded','failed')),
    attempts integer NOT NULL CHECK (attempts > 0),
    started_at timestamptz NOT NULL,
    finished_at timestamptz,
    PRIMARY KEY(job_name, scheduled_at)
);
REVOKE ALL ON public.platform_job_runs FROM PUBLIC;
COMMIT;
