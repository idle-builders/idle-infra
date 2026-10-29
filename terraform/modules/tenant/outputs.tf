output "namespace" {
  value       = kubernetes_namespace.tenant.metadata[0].name
  description = "Kubernetes namespace name for this tenant."
}

output "database_url" {
  value       = "postgresql://${postgresql_role.tenant.name}:${var.tenant_db_password}@${var.pg_host}:${var.pg_port}/${var.database_name}?sslmode=verify-ca&sslrootcert=/etc/ssl/pg/ca.crt&options=-csearch_path%3D${var.tenant_name}"
  sensitive   = true
  description = "PostgreSQL connection URL for this tenant (includes search_path)."
}

output "deployer_service_account" {
  value       = kubernetes_service_account.deployer.metadata[0].name
  description = "Name of the CI/CD deployer ServiceAccount in this tenant's namespace."
}

output "pg_role_name" {
  value       = postgresql_role.tenant.name
  description = "PostgreSQL role name for this tenant."
}

output "pg_schema_name" {
  value       = postgresql_schema.tenant.name
  description = "PostgreSQL schema name for this tenant."
}

