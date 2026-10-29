variable "namespace" {
  type        = string
  default     = "infra"
  description = "Namespace the console runs in (shared services). Egress to the K8s API and DNS is provided by the infra-wide network policies."
}

variable "image" {
  type        = string
  description = "Digest-pinned infra-console image (ghcr.io/idle-builders/idle-infra/infra-console@sha256:...)."

  # Enforced, not just described: with a mutable tag and IfNotPresent, two
  # replicas can serve different code indefinitely, and re-pinning the same tag
  # never re-pulls.
  validation {
    condition     = var.image == "" || can(regex("@sha256:[0-9a-f]{64}$", var.image))
    error_message = "image must be digest-pinned (...@sha256:<64 hex>), or empty to disable the module."
  }
}

variable "tenant_namespaces" {
  type        = set(string)
  default     = []
  description = <<-EOT
    Tenant namespaces the console may read ConfigMaps and pod logs in.

    These two are granted per namespace rather than cluster-wide. Helm release
    ConfigMaps carry the whole rendered release (which can include templated
    Secret objects) and pod logs routinely contain tokens, so a cluster-wide
    grant would hand anyone holding the ServiceAccount token far more than any
    code path here reads.
  EOT
}

variable "node_selector" {
  type        = map(string)
  default     = {}
  description = "Node labels required for infra-console pod placement."
}

variable "replicas" {
  type        = number
  default     = 1
  description = "Replica count. The read-only console tolerates brief maintenance downtime."
}

variable "ghcr_pull_secret_name" {
  type        = string
  default     = "ghcr-pull"
  description = "Name of the dockerconfigjson pull secret in var.namespace for the private GHCR image."
}

variable "console_domain" {
  type        = string
  description = "Hostname the console is served on (e.g. console.example.com). Also the OIDC redirect host."
}

# --- Optional upstreams ------------------------------------------------------
# Every one of these may be empty. The console reports an unavailable Section
# for an unconfigured source rather than refusing to start: a console that
# cannot boot because Cloudflare is unreachable is worse than one showing an
# unavailable panel.

variable "github_token" {
  type        = string
  default     = ""
  sensitive   = true
  description = "Read-only GitHub token. Enriches the deployment timeline with actor and duration; empty leaves those fields absent."
}

variable "cloudflare_api_token" {
  type        = string
  default     = ""
  sensitive   = true
  description = "Read-only Cloudflare token for DNS and tunnel status."
}

variable "cloudflare_account_id" {
  type        = string
  default     = ""
  description = "Cloudflare account id, required for tunnel status."
}

variable "oidc_issuer" {
  type        = string
  description = <<-EOT
    OIDC issuer that authenticates console and MCP callers (e.g.
    https://id.example.com). Tokens are verified through its USERINFO endpoint,
    because Zitadel's access tokens and PATs are opaque JWE.

    Required: an empty value disables authentication, which is only correct for
    local development.
  EOT

  validation {
    condition     = can(regex("^https://", var.oidc_issuer))
    error_message = "oidc_issuer must be an https:// URL — an empty or plaintext issuer would serve the console unauthenticated."
  }
}

variable "oidc_client_id" {
  type        = string
  default     = ""
  description = <<-EOT
    Client id of the console's Zitadel application (PKCE public client — there
    is no secret). Enables browser sign-in at /auth/login.

    Register the application in Zitadel with redirect URI
    https://<console_domain>/auth/callback. Empty leaves the API reachable by
    bearer token while the UI cannot sign in.
  EOT
}

variable "zitadel_url" {
  type        = string
  default     = ""
  description = "Zitadel base URL (e.g. https://id.example.com)."
}

variable "zitadel_token" {
  type        = string
  default     = ""
  sensitive   = true
  description = "Zitadel service-user PAT. Presented as a bearer token; only verifying a Zitadel token needs USERINFO."
}

# --- Object storage (S3, same convention as the storage broker) -------------
#
# S3 rather than Swift's native API: every other component that touches
# Infomaniak object storage speaks S3 (the storage broker, the PostgreSQL
# backups, the OpenTofu state backend), and the credential is minted by
# OpenTofu as an EC2 credential rather than created by hand.
variable "s3_endpoint" {
  type        = string
  default     = "https://s3.pub2.infomaniak.cloud"
  description = "Infomaniak S3-compatible endpoint. pub2 is dc4-a; pub1 is dc3-a."
}

variable "s3_region" {
  type        = string
  default     = "us-east-1"
  description = "S3 region string (Infomaniak ignores it)."
}

variable "s3_access_key_id" {
  type        = string
  default     = ""
  sensitive   = true
  description = "S3 access key for the on-demand telemetry query helper. Exact bucket totals use Swift metadata below."
}

variable "s3_secret_access_key" {
  type        = string
  default     = ""
  sensitive   = true
  description = "Secret for var.s3_access_key_id."
}

# --- Exact object-storage metadata (Swift) ----------------------------------

variable "swift_auth_url" {
  type        = string
  default     = ""
  description = "Keystone v3 endpoint used to authenticate the metadata-only Swift client."
}

variable "swift_endpoint" {
  type        = string
  default     = ""
  description = "Project-scoped Swift endpoint whose container HEAD returns exact counters."
}

variable "swift_application_credential_id" {
  type        = string
  default     = ""
  sensitive   = true
  description = "OpenTofu-generated application credential ID for Swift container metadata."
}

variable "swift_application_credential_secret" {
  type        = string
  default     = ""
  sensitive   = true
  description = "Secret for var.swift_application_credential_id."
}

# --- PostgreSQL (platform_reader) --------------------------------------------
variable "postgres_host" {
  type        = string
  default     = ""
  description = "PostgreSQL host for the database panel. Empty leaves that panel unavailable."
}

variable "postgres_database" {
  type        = string
  default     = "postgres"
  description = "Database holding the tenant schemas."
}

variable "postgres_user" {
  type        = string
  default     = ""
  description = "Read-only role the console connects as. This is platform_reader: SELECT and nothing else, granted per tenant schema."
}

variable "postgres_password" {
  type        = string
  default     = ""
  sensitive   = true
  description = "Password for var.postgres_user."
}

variable "pg_ca_enabled" {
  type        = bool
  default     = false
  description = "Mount pg_ca_cert. A plan-time flag, not derived from the certificate: the CA is read from a data source that depends on the PostgreSQL release, so its value is unknown at plan whenever that release changes, and count cannot depend on it."
}

variable "pg_ca_cert" {
  type        = string
  default     = ""
  sensitive   = true
  description = "PostgreSQL CA certificate, mounted at /etc/ssl/pg/ca.crt so the console can use sslmode=verify-ca — the same convention as the tenant DATABASE_URL. Empty disables the mount."
}

variable "log_level" {
  type        = string
  default     = "info"
  description = "slog level: debug, info, warn or error."
}

variable "resources" {
  type = object({
    requests = object({ cpu = string, memory = string })
    limits   = object({ cpu = string, memory = string })
  })
  default = {
    requests = { cpu = "10m", memory = "32Mi" }
    limits   = { cpu = "100m", memory = "96Mi" }
  }
  description = "Container resources. The console is a stateless facade; its memory is dominated by cached API responses."
}
