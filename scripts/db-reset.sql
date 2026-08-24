-- Drops and recreates both databases. Destructive — development only.
--     npm run db:reset
--
-- FORCE (Postgres 13+) terminates existing connections rather than failing with
-- "database is being accessed by other users", which is otherwise the most
-- common annoyance when resetting while the API is still running.

\set ON_ERROR_STOP on

DROP DATABASE IF EXISTS fleet WITH (FORCE);
DROP DATABASE IF EXISTS fleet_test WITH (FORCE);

CREATE DATABASE fleet OWNER fleet;
CREATE DATABASE fleet_test OWNER fleet;

\echo 'Databases recreated empty. Run `npm run db:migrate` next.'
