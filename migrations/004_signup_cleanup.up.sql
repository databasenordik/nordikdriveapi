-- Additive expiry indexes only. Run with the existing migration identity.
BEGIN;
SET LOCAL lock_timeout = '2s';
SET LOCAL statement_timeout = '30s';
CREATE INDEX IF NOT EXISTS signup_limits_expiry ON public.signup_otp_limits(expires_at,key);
CREATE INDEX IF NOT EXISTS signup_idempotency_expiry ON public.signup_idempotency(expires_at,key);
CREATE INDEX IF NOT EXISTS signup_challenge_expiry ON public.signup_otp_challenges
 ((GREATEST(expires_at,COALESCE(proof_expires_at,expires_at))),id);
COMMIT;
