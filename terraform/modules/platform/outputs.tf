output "namespace" {
  value = kubernetes_namespace.infra.metadata[0].name
}

output "observability_namespace" {
  value = try(kubernetes_namespace.observability[0].metadata[0].name, null)
}

output "postgresql_ca_cert" {
  value     = data.kubernetes_secret.postgresql_ca.data["ca.crt"]
  sensitive = true
}

output "backup_role_name" {
  value = postgresql_role.pg_backup.name
}

output "reader_role_name" {
  value = postgresql_role.platform_reader.name
}

output "reader_password" {
  value     = random_password.platform_reader_password.result
  sensitive = true
}

output "storage_broker_password" {
  value     = random_password.storage_broker_database.result
  sensitive = true
}

output "storage_broker_role_name" {
  value = postgresql_role.storage_broker.name
}

output "storage_broker_s3_access_key" {
  value     = openstack_identity_ec2_credential_v3.storage_broker.access
  sensitive = true
}

output "storage_broker_s3_secret_key" {
  value     = openstack_identity_ec2_credential_v3.storage_broker.secret
  sensitive = true
}

output "console_s3_access_key" {
  value     = try(openstack_identity_ec2_credential_v3.infra_console[0].access, null)
  sensitive = true
}

output "console_s3_secret_key" {
  value     = try(openstack_identity_ec2_credential_v3.infra_console[0].secret, null)
  sensitive = true
}

output "console_swift_credential_id" {
  value     = try(openstack_identity_application_credential_v3.infra_console_swift[0].id, null)
  sensitive = true
}

output "console_swift_project_id" {
  value = try(openstack_identity_application_credential_v3.infra_console_swift[0].project_id, null)
}

output "console_swift_secret" {
  value     = try(openstack_identity_application_credential_v3.infra_console_swift[0].secret, null)
  sensitive = true
}

output "observability_pull_secret_name" {
  value = try(kubernetes_secret.ghcr_pull_observability[0].metadata[0].name, null)
}
