-- Manual paper reset. TRUNCATE below deletes jev_arb_runs and cascaded paper rows.
-- Do not add this file to schema migrations, and do not run it from Compose.
-- Stop jev-service, apply migration 060, run this script by hand, then start V6.
BEGIN;
TRUNCATE jev_arb_runs RESTART IDENTITY CASCADE;
COMMIT;
