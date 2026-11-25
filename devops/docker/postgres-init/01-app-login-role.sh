#!/bin/sh
# Runs once, when the PostgreSQL volume is first initialised.
#
# Creates the login role the API, worker and fincorectl connect as. It owns
# nothing and has no privileges of its own: everything comes from membership
# of fincore_app, whose table grants are made by migration 00008. The schema
# owner (POSTGRES_USER) is used only by the migrate step.
set -eu
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" \
     -v app_user="$APP_DB_USER" -v app_password="$APP_DB_PASSWORD" <<'SQL'
SELECT 'CREATE ROLE fincore_app NOLOGIN'
 WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'fincore_app') \gexec
CREATE ROLE :"app_user" LOGIN PASSWORD :'app_password' NOSUPERUSER NOCREATEDB NOCREATEROLE;
GRANT fincore_app TO :"app_user";
SQL
