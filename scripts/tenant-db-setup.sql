-- =============================================================================
-- scripts/tenant-db-setup.sql
-- =============================================================================
--
-- Reference template: SQL commands to set up a tenant schema and role.
--
-- STATUS: This is a documentation/recovery template.
-- The actual schema and role provisioning is managed by Terraform using the
-- cyrilgdn/postgresql provider (terraform/main/modules/tenant/main.tf).
-- Run Terraform instead of executing this SQL manually.
--
-- Use this script only for:
--   (a) Understanding what Terraform is doing
--   (b) Manual recovery if Terraform state is lost
--   (c) Local development testing
--
-- Replace:
--   :tenant_name     — tenant identifier (e.g. beacon, atlas, acme)
--   :tenant_password — secure password for the tenant role
--   :database_name   — PostgreSQL database (default: postgres)
-- =============================================================================

-- Connect as the postgres superuser before running this script.
-- psql -h localhost -U postgres -d postgres -f tenant-db-setup.sql

-- Create the tenant role with login and password
CREATE ROLE :tenant_name WITH LOGIN PASSWORD ':tenant_password';

-- Create the schema owned by the tenant role
CREATE SCHEMA :tenant_name AUTHORIZATION :tenant_name;

-- Grant full access to the tenant's own schema
GRANT CREATE, USAGE ON SCHEMA :tenant_name TO :tenant_name;

-- Revoke default access to the public schema.
-- PostgreSQL grants USAGE and CREATE on public to all roles by default.
-- Without this revocation, the tenant role can read/write the public schema.
REVOKE ALL ON SCHEMA public FROM :tenant_name;

-- Verify: connect as the tenant role and check accessible schemas
-- psql -h localhost -U :tenant_name -d :database_name -c "\dn"

-- =============================================================================
-- Cleanup (for testing only — do not run in production)
-- =============================================================================
-- DROP SCHEMA :tenant_name CASCADE;
-- DROP ROLE :tenant_name;

