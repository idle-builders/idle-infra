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
  default     = "https://s3.swiss-backup04.infomaniak.com"
  description = "S3-compatible endpoint for Swiss Backup DR storage (db-backup device)."
}

variable "backup_s3_secret_access_key" {
  type        = string
  sensitive   = true
  description = "S3 secret access key for PostgreSQL backups. MUST use a separate credential pair from the Terraform state backend."
}

variable "escrow_s3_access_key_id" {
  type        = string
  sensitive   = true
  description = "S3 access key for the infra-backup Swiss Backup device (key escrow)."
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

variable "identity_domain" {
  type        = string
  description = "Public hostname for the shared Zitadel identity provider. Served via the Cloudflare Tunnel; Cloudflare terminates TLS at the edge."
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
}

variable "kubeconfig_path" {
  type        = string
  description = "Path to the kubeconfig file for the Infomaniak K8s cluster."
}

variable "letsencrypt_email" {
  type        = string
  description = "Email address for Let's Encrypt ACME registration."
}

variable "observability_enabled" {
  type        = bool
  default     = true
  description = "Deploy the O1 collectors and compactor."
}

variable "pg_admin_password" {
  type        = string
  sensitive   = true
  description = "PostgreSQL superuser (postgres) password for the self-hosted PG instance."
}

variable "pg_backup_suspended" {
  type        = bool
  default     = false
  description = "Suspend scheduled PostgreSQL backups when the deployment operator explicitly disables them."
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

variable "apiserver_cidr" {
  type        = string
  description = "Post-DNAT Kubernetes API endpoint CIDR for network policy."
}

variable "apiserver_port" {
  type        = number
  description = "Post-DNAT Kubernetes API endpoint port."
}

variable "node_selector" {
  type    = map(string)
  default = {}
}

variable "helm_values" {
  type        = map(string)
  default     = {}
  description = "Optional complete values documents keyed by chart; defaults come from this framework revision."
}

variable "postgresql_storage_class" {
  type    = string
  default = "csi-cinder-sc-retain"
}

variable "postgresql_storage_size" {
  type    = string
  default = "10Gi"
}

variable "identity_admin" {
  type = object({ organization = string, email = string, first_name = string, last_name = string })
}

variable "issuer_manifest_path" {
  type        = string
  description = "Writable path for the generated ACME issuer manifest, outside the framework checkout."
}

variable "escrow_s3_bucket" {
  type    = string
  default = "default"
}

variable "escrow_s3_endpoint" {
  type = string
}
