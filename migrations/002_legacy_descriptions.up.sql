-- Prepare the two columns historically created by the upload handler before
-- switching the monolith to a runtime identity without ALTER privileges.
BEGIN;
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';
DO $$
BEGIN
    IF to_regclass('public.file') IS NOT NULL THEN
        ALTER TABLE public.file ADD COLUMN IF NOT EXISTS description text;
    END IF;
    IF to_regclass('public.file_version') IS NOT NULL THEN
        ALTER TABLE public.file_version ADD COLUMN IF NOT EXISTS description text;
    END IF;
END
$$;
COMMIT;
