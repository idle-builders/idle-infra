variable "apiserver_cidr" {
  type        = string
  description = "Post-DNAT Kubernetes API endpoint CIDR for network policy."
}

variable "apiserver_port" {
  type        = number
  description = "Post-DNAT Kubernetes API endpoint port."
}

variable "availability_zone" {
  type    = string
  default = "az-1"
}

variable "backup_s3_access_key_id" {
  type        = string
  sensitive   = true
  description = "S3 access key ID for PostgreSQL backups. MUST use a separate credential pair from the Terraform state backend."
}

variable "backup_s3_bucket" {
  type        = string
  default     = "default"
  description = "S3 bucket name for Swiss Backup db-backup device. Swiss Backup uses 'default' as the bucket name."
}

variable "backup_s3_endpoint" {
  type        = string
  description = "S3-compatible endpoint for Swiss Backup DR storage (db-backup device)."
}

variable "backup_s3_secret_access_key" {
  type        = string
  sensitive   = true
  description = "S3 secret access key for PostgreSQL backups. MUST use a separate credential pair from the Terraform state backend."
}

variable "broker_oidc_audiences" {
  type    = list(string)
  default = []
}

variable "cloudflare_account_id" {
  type        = string
  sensitive   = true
  default     = ""
  description = "Cloudflare account ID. Used to enumerate/scope zones for DNS management."
}

variable "cloudflare_api_token" {
  type        = string
  default     = ""
  sensitive   = true
  description = "Read-only Cloudflare token for DNS and tunnel status."
}

variable "cluster_name" {
  type    = string
  default = "infra"
}

variable "cluster_pack" {
  type    = string
  default = "shared"
}

variable "console_domain" {
  type        = string
  description = "Hostname the console is served on (e.g. console.example.com). Also the OIDC redirect host."
}

variable "console_oidc_client_id" {
  type        = string
  default     = ""
  description = <<-EOT
    Client id of the console's Zitadel application, a PKCE public client (no
    secret). Enables browser sign-in.

    Create it in Zitadel: an application of type User Agent / SPA with PKCE,
    redirect URI https://<console_domain>/auth/callback. Not sensitive — a
    public client id is not a credential.
  EOT
}

variable "escrow_s3_access_key_id" {
  type        = string
  sensitive   = true
  description = "S3 access key for the infra-backup Swiss Backup device (key escrow)."
}

variable "escrow_s3_endpoint" {
  type = string
}

variable "escrow_s3_secret_access_key" {
  type        = string
  sensitive   = true
  description = "S3 secret access key for the infra-backup Swiss Backup device (key escrow)."
}

variable "ghcr_token" {
  type        = string
  sensitive   = true
  description = "GitHub PAT (read:packages plus package access) for GHCR image pull access."
}

variable "ghcr_username" {
  type        = string
  description = "GitHub username for GHCR image pull access."
}

variable "helm_values" {
  type    = map(string)
  default = {}
}

variable "identity_admin" {
  type = object({ organization = string, email = string, first_name = string, last_name = string })
}

variable "identity_domain" {
  type        = string
  description = "Public hostname for the shared Zitadel identity provider. Served via the Cloudflare Tunnel; Cloudflare terminates TLS at the edge."
}

variable "infomaniak_project_id" {
  type        = number
  description = "Infomaniak project ID."
}

variable "infomaniak_public_cloud_id" {
  type        = number
  description = "Infomaniak public cloud ID."
}

variable "infra_console_image" {
  type        = string
  description = <<-EOT
    Container image for the infra-console (read-only console and MCP surface
    over platform state). Built by .github/workflows/build-infra-console.yml
    and pushed to GHCR. Pin by digest.

    Empty disables the console entirely — the module is instantiated with
    count = 0 — so the platform can be applied before the first image exists.
  EOT
  default     = ""
}

variable "keda_enabled" {
  type    = bool
  default = false
}

variable "keda_values" {
  type    = string
  default = ""
}

variable "kubeconfig_path" {
  type        = string
  description = "Path to the kubeconfig file for the Infomaniak K8s cluster."
}

variable "kubernetes_version" {
  type        = string
  default     = "1.35"
  description = "Kubernetes version for the managed cluster."
}

variable "letsencrypt_email" {
  type        = string
  description = "Email address for Let's Encrypt ACME registration."
}

variable "node_pool_flavor" {
  type        = string
  default     = "a4-ram8-disk50-perf1"
  description = "Infomaniak worker flavor (4 vCPU, 8 GB RAM, 50 GB disk)."
}

variable "node_pool_max_size" {
  type        = number
  default     = 4
  description = "Maximum number of nodes in the K8s node pool. Set equal to min for fixed size, higher to enable autoscaling."
}

variable "node_pool_min_size" {
  type        = number
  default     = 3
  description = "Minimum number of nodes in the K8s node pool."
}

variable "node_pool_name" {
  type    = string
  default = "workers"
}

variable "node_selector" {
  type        = map(string)
  default     = {}
  description = "Node labels required for storage-broker pod placement."
}

variable "observability_enabled" {
  type        = bool
  default     = false
  description = "Deploy the O1 collectors and compactor."
}

variable "otel_collector_image" {
  type        = string
  description = "Digest-pinned custom minimal OpenTelemetry Collector image."
}

variable "pg_admin_password" {
  type        = string
  sensitive   = true
  description = "PostgreSQL superuser (postgres) password for the self-hosted PG instance."
}

variable "pg_backup_image" {
  type        = string
  description = "Container image for the in-cluster PostgreSQL backup CronJob."
}

variable "pg_replication_password" {
  type        = string
  sensitive   = true
  description = "PostgreSQL replication password (used by read replica to connect to primary)."
}

variable "postgresql_admin_port" {
  type        = number
  default     = 5432
  description = "Local port forwarded to PostgreSQL for OpenTofu provider access. Override when 5432 is already in use."
}

variable "postgresql_storage_class" {
  type    = string
  default = "csi-cinder-sc-retain"
}

variable "postgresql_storage_size" {
  type    = string
  default = "10Gi"
}

variable "region" {
  type        = string
  default     = "dc4-a"
  description = "Infomaniak OpenStack region for the Swift container."
}

variable "state_encryption_passphrase" {
  type        = string
  sensitive   = true
  description = "Passphrase for OpenTofu AES-256-GCM state encryption via PBKDF2 key derivation."
}

variable "storage_broker_image" {
  type        = string
  description = <<-EOT
    Container image for the storage-broker (legacy presign plus governed object
    authorization). Built by .github/workflows/build-storage-broker.yml and pushed to
    GHCR. Digest-pinned (like cloudflared); re-pin after each image rebuild by
    reading the published digest from GHCR.
  EOT
}

variable "telemetry_data_image" {
  type        = string
  description = "Digest-pinned telemetry Parquet compactor image."
}

variable "tenant_db_passwords" {
  type      = map(string)
  sensitive = true
  default   = {}
}

variable "tenants" {
  type    = map(object({ allowed_ingress_domains = list(string) }))
  default = {}
}

variable "zitadel_admin_db_password" {
  type        = string
  sensitive   = true
  description = "PostgreSQL password for the zitadel_admin role (migrations and setup)."
}

variable "zitadel_admin_password" {
  type        = string
  sensitive   = true
  description = "Initial Zitadel console admin password (FirstInstance human user, admin@example.com). Stored in Keychain/escrow/GitHub like other infra credentials."
}

variable "zitadel_db_password" {
  type        = string
  sensitive   = true
  description = "PostgreSQL password for the zitadel role (identity provider database)."
}

variable "zitadel_masterkey" {
  type        = string
  sensitive   = true
  description = "AES-256 encryption key for Zitadel data at rest. Must be exactly 32 characters. Generate with: openssl rand -base64 24 | head -c 32"

  validation {
    condition     = length(var.zitadel_masterkey) == 32
    error_message = "Zitadel masterkey must be exactly 32 characters."
  }
}

variable "application_telemetry_sources" {
  description = "Explicit workload identities for optional bounded application execution summaries."
  type = list(object({
    k8s_namespace     = string
    service_namespace = string
    service_name      = string
    workload          = optional(string)
    cronjob           = optional(string)
  }))
  default = []
}
