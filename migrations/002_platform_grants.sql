-- psql variables are IAM DB usernames (service-account email without
-- .gserviceaccount.com). Run as an identity allowed to grant existing objects.
-- psql ... -v gateway_user=... -v legacy_user=... -v worker_user=... -f ...
\set ON_ERROR_STOP on
BEGIN;
GRANT USAGE ON SCHEMA gateway TO :"gateway_user";
GRANT SELECT, INSERT, UPDATE, DELETE ON gateway.rate_limits TO :"gateway_user";
-- These two identities temporarily run the existing monolith's workflows.
-- Revoke these transitional grants as each owning service is extracted.
GRANT USAGE ON SCHEMA public TO :"legacy_user", :"worker_user";
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO :"legacy_user", :"worker_user";
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO :"legacy_user", :"worker_user";
COMMIT;
