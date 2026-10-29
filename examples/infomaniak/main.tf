module "cluster" {
  source                     = "../../terraform/modules/cluster"
  availability_zone          = var.availability_zone
  cluster_name               = var.cluster_name
  cluster_pack               = var.cluster_pack
  infomaniak_project_id      = var.infomaniak_project_id
  infomaniak_public_cloud_id = var.infomaniak_public_cloud_id
  kubernetes_version         = var.kubernetes_version
  node_labels                = var.node_selector
  node_pool_flavor           = var.node_pool_flavor
  node_pool_max_size         = var.node_pool_max_size
  node_pool_min_size         = var.node_pool_min_size
  node_pool_name             = var.node_pool_name
  region                     = var.region
}

module "platform" {
  source = "../../terraform/modules/platform"

  apiserver_cidr              = var.apiserver_cidr
  apiserver_port              = var.apiserver_port
  backup_s3_access_key_id     = var.backup_s3_access_key_id
  backup_s3_bucket            = var.backup_s3_bucket
  backup_s3_endpoint          = var.backup_s3_endpoint
  backup_s3_secret_access_key = var.backup_s3_secret_access_key
  escrow_s3_access_key_id     = var.escrow_s3_access_key_id
  escrow_s3_bucket            = "default"
  escrow_s3_endpoint          = var.escrow_s3_endpoint
  escrow_s3_secret_access_key = var.escrow_s3_secret_access_key
  ghcr_token                  = var.ghcr_token
  ghcr_username               = var.ghcr_username
  helm_values                 = var.helm_values
  identity_admin              = var.identity_admin
  identity_domain             = var.identity_domain
  infra_console_image         = var.infra_console_image
  issuer_manifest_path        = "${path.module}/.terraform/tmp/clusterissuer-letsencrypt.yaml"
  kubeconfig_path             = var.kubeconfig_path
  letsencrypt_email           = var.letsencrypt_email
  node_selector               = var.node_selector
  observability_enabled       = var.observability_enabled
  pg_admin_password           = var.pg_admin_password
  pg_backup_image             = var.pg_backup_image
  pg_replication_password     = var.pg_replication_password
  postgresql_storage_class    = var.postgresql_storage_class
  postgresql_storage_size     = var.postgresql_storage_size
  zitadel_admin_db_password   = var.zitadel_admin_db_password
  zitadel_admin_password      = var.zitadel_admin_password
  zitadel_db_password         = var.zitadel_db_password
  zitadel_masterkey           = var.zitadel_masterkey

  depends_on = [module.cluster]
}

module "edge" {
  source                = "../../terraform/modules/edge"
  namespace             = module.platform.namespace
  cloudflare_account_id = var.cloudflare_account_id
  node_selector         = var.node_selector
  depends_on            = [module.cluster]
}

module "keda" {
  count          = var.keda_enabled ? 1 : 0
  source         = "../../terraform/modules/keda"
  apiserver_cidr = var.apiserver_cidr
  apiserver_port = var.apiserver_port
  helm_values    = var.keda_values
  depends_on     = [module.cluster]
}

# Tenant inventory is independent of secret values, so sensitive passwords never become instance keys.
module "tenant" {
  for_each                = var.tenants
  source                  = "../../terraform/modules/tenant"
  tenant_name             = each.key
  tenant_db_password      = var.tenant_db_passwords[each.key]
  allowed_ingress_domains = each.value.allowed_ingress_domains
  database_name           = "postgres"
  pg_host                 = "postgresql-primary.infra.svc.cluster.local"
  pg_ca_cert              = module.platform.postgresql_ca_cert
  ghcr_username           = var.ghcr_username
  ghcr_token              = var.ghcr_token
}

module "object_storage" {
  for_each    = var.tenants
  source      = "../../terraform/modules/object-storage"
  tenant_name = each.key
  region      = var.region
  depends_on  = [module.tenant]
}

module "storage_broker" {
  source               = "../../terraform/modules/storage-broker"
  namespace            = module.platform.namespace
  image                = var.storage_broker_image
  node_selector        = var.node_selector
  oidc_issuer          = "https://${var.identity_domain}"
  oidc_audiences       = var.broker_oidc_audiences
  s3_access_key_id     = module.platform.storage_broker_s3_access_key
  s3_secret_access_key = module.platform.storage_broker_s3_secret_key
  database_url         = "postgresql://${module.platform.storage_broker_role_name}:${module.platform.storage_broker_password}@postgresql-primary.infra.svc.cluster.local:5432/postgres?sslmode=verify-ca&sslrootcert=/etc/ssl/pg/ca.crt&options=-csearch_path%3Dstorage_broker"
  pg_ca_cert           = module.platform.postgresql_ca_cert
  governed_policies    = {}
  tenant_policies = { for name, tenant in var.tenants : name => {
    bucket             = "${name}-files"
    ops                = ["get", "put", "delete"]
    require_user_token = true
    prefixes           = ["u/$${sub}/"]
  } }
  depends_on = [module.object_storage]
}

# Backups and the read-only console get explicit per-tenant grants.
resource "postgresql_grant" "backup_schema" {
  for_each    = var.tenants
  database    = "postgres"
  role        = module.platform.backup_role_name
  schema      = module.tenant[each.key].pg_schema_name
  object_type = "schema"
  privileges  = ["USAGE"]
}
resource "postgresql_grant" "backup_tables" {
  for_each    = var.tenants
  database    = "postgres"
  role        = module.platform.backup_role_name
  schema      = module.tenant[each.key].pg_schema_name
  object_type = "table"
  privileges  = ["SELECT"]
  depends_on  = [postgresql_grant.backup_schema]
}
resource "postgresql_grant" "backup_sequences" {
  for_each    = var.tenants
  database    = "postgres"
  role        = module.platform.backup_role_name
  schema      = module.tenant[each.key].pg_schema_name
  object_type = "sequence"
  privileges  = ["SELECT"]
  depends_on  = [postgresql_grant.backup_schema]
}
resource "postgresql_default_privileges" "backup_future_tables" {
  for_each    = var.tenants
  database    = "postgres"
  owner       = module.tenant[each.key].pg_role_name
  role        = module.platform.backup_role_name
  schema      = module.tenant[each.key].pg_schema_name
  object_type = "table"
  privileges  = ["SELECT"]
  depends_on  = [postgresql_grant.backup_schema]
}
resource "postgresql_default_privileges" "backup_future_sequences" {
  for_each    = var.tenants
  database    = "postgres"
  owner       = module.tenant[each.key].pg_role_name
  role        = module.platform.backup_role_name
  schema      = module.tenant[each.key].pg_schema_name
  object_type = "sequence"
  privileges  = ["SELECT"]
  depends_on  = [postgresql_grant.backup_schema]
}
module "infra_console" {
  count                = var.infra_console_image == "" ? 0 : 1
  source               = "../../terraform/modules/infra-console"
  namespace            = module.platform.namespace
  image                = var.infra_console_image
  node_selector        = var.node_selector
  console_domain       = var.console_domain
  oidc_issuer          = "https://${var.identity_domain}"
  oidc_client_id       = var.console_oidc_client_id
  tenant_namespaces    = keys(var.tenants)
  postgres_host        = "postgresql-primary.infra.svc.cluster.local"
  postgres_database    = "postgres"
  postgres_user        = module.platform.reader_role_name
  postgres_password    = module.platform.reader_password
  pg_ca_cert           = module.platform.postgresql_ca_cert
  pg_ca_enabled        = true
  s3_access_key_id     = module.platform.console_s3_access_key
  s3_secret_access_key = module.platform.console_s3_secret_key
  depends_on           = [module.tenant]
}
resource "postgresql_grant" "reader_schema" {
  for_each    = var.tenants
  database    = "postgres"
  role        = module.platform.reader_role_name
  schema      = module.tenant[each.key].pg_schema_name
  object_type = "schema"
  privileges  = ["USAGE"]
}
resource "postgresql_default_privileges" "reader_future_tables" {
  for_each    = var.tenants
  database    = "postgres"
  owner       = module.tenant[each.key].pg_role_name
  role        = module.platform.reader_role_name
  schema      = module.tenant[each.key].pg_schema_name
  object_type = "table"
  privileges  = ["SELECT"]
  depends_on  = [postgresql_grant.reader_schema]
}

module "observability" {
  count                         = var.observability_enabled ? 1 : 0
  source                        = "../../terraform/modules/observability"
  namespace                     = module.platform.observability_namespace
  collector_image               = var.otel_collector_image
  compactor_image               = var.telemetry_data_image
  ghcr_pull_secret_name         = module.platform.observability_pull_secret_name
  collector_config              = file("${path.module}/../../docker/otel-collector/config.yaml")
  apiserver_cidr                = var.apiserver_cidr
  apiserver_port                = var.apiserver_port
  otlp_ingress_namespaces       = concat([module.platform.namespace], keys(var.tenants))
  application_telemetry_sources = var.application_telemetry_sources
  depends_on                    = [module.tenant]
}
