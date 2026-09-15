-- Creates the development role and databases. Run once, as the postgres superuser:
--     npm run db:setup
--
-- The application connects as `fleet`, never as `postgres`. Running an app as a
-- superuser hides permission bugs until they surface in production.

\set ON_ERROR_STOP on

-- CREATE ROLE has no IF NOT EXISTS, so guard it. The DO block is plpgsql, which
-- is available in every Postgres install by default.
DO $$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'fleet') THEN
    CREATE ROLE fleet WITH LOGIN PASSWORD 'fleet';
  END IF;
END
$$;

-- CREATE DATABASE cannot run inside a transaction block or a DO block, so these
-- are plain statements. \gexec runs the SELECT's result as SQL, which gives us a
-- conditional create without erroring when the database already exists.
SELECT 'CREATE DATABASE fleet OWNER fleet'
 WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'fleet')\gexec

SELECT 'CREATE DATABASE fleet_test OWNER fleet'
 WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'fleet_test')\gexec

-- A third database purely so the store tests and the end-to-end pipeline tests
-- do not share one. `go test ./...` runs packages in parallel, and both suites
-- truncate; pointed at the same database they delete each other's rows mid-run
-- and fail in ways that look like flakiness. Separate databases remove the
-- shared state rather than papering over it with `-p 1`, which only works for
-- whoever remembers the flag.
SELECT 'CREATE DATABASE fleet_pipeline_test OWNER fleet'
 WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'fleet_pipeline_test')\gexec

\echo 'Databases ready: fleet, fleet_test, fleet_pipeline_test (owner: fleet)'
