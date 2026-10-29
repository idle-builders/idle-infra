variable "namespace" {
  type        = string
  default     = "infra"
  description = "Namespace the broker runs in (shared services). Egress to the K8s API and DNS is provided by the infra-wide network policies."
}

variable "image" {
  type        = string
  description = "Digest-pinned storage-broker image (ghcr.io/idle-builders/idle-infra/storage-broker@sha256:...)."
}

variable "node_selector" {
  type        = map(string)
  default     = {}
  description = "Node labels required for storage-broker pod placement."
}

variable "replicas" {
  type        = number
  default     = 2
  description = "Replica count. Stateless; ≥2 for availability behind a PDB."
}

variable "ghcr_pull_secret_name" {
  type        = string
  default     = "ghcr-pull"
  description = "Name of the dockerconfigjson pull secret in var.namespace for the private GHCR image."
}

# --- S3 (the ONLY place the credential lives) ------------------------------
variable "s3_endpoint" {
  type        = string
  default     = "https://s3.pub2.infomaniak.cloud"
  description = "Infomaniak S3-compatible endpoint."
}

variable "s3_region" {
  type        = string
  default     = "us-east-1"
  description = "S3 region string (Infomaniak ignores it)."
}

variable "s3_access_key_id" {
  type        = string
  sensitive   = true
  description = "Access key of the broker's dedicated S3 credential."
}

variable "s3_secret_access_key" {
  type        = string
  sensitive   = true
  description = "Secret key of the broker's dedicated S3 credential."
}

# --- Zitadel / OIDC user-token verification --------------------------------
variable "oidc_issuer" {
  type        = string
  description = "OIDC issuer URL (e.g. https://id.example.com). JWKS is discovered from it."
}

variable "oidc_audiences" {
  type        = list(string)
  default     = []
  description = "Allowed audiences for user access tokens. Empty = accept any audience from the issuer (signature/issuer/expiry still enforced)."
}

variable "token_audience" {
  type        = string
  default     = "storage-broker"
  description = "Audience the app's projected ServiceAccount token must carry (TokenReview)."
}

variable "database_url" {
  type        = string
  sensitive   = true
  description = "Dedicated PostgreSQL connection URL for broker-owned object and execution-grant rows."
}

variable "pg_ca_cert" {
  type        = string
  sensitive   = true
  description = "PostgreSQL CA certificate mounted for sslmode=verify-ca."
}

# --- Authorization policy --------------------------------------------------
variable "default_ttl_seconds" {
  type = object({
    get    = number
    put    = number
    delete = number
  })
  default = {
    get    = 900
    put    = 300
    delete = 120
  }
  description = "Presigned-URL lifetimes per operation."
}

variable "tenant_policies" {
  type = map(object({
    bucket             = string
    ops                = list(string)
    require_user_token = bool
    prefixes           = list(string)
  }))
  description = <<-EOT
    Per-namespace authorization. Key = tenant namespace (the verified k8s identity).
    `prefixes` entries may use the literal placeholder $${sub} (escaped here so
    Terraform passes it through verbatim), which the broker substitutes with the
    verified Zitadel subject to bind a key to one user. A prefix without a
    placeholder is an app-layer (shared) prefix.
  EOT

  validation {
    condition     = length(distinct([for policy in values(var.tenant_policies) : policy.bucket])) == length(var.tenant_policies)
    error_message = "Each tenant policy must use a distinct physical bucket."
  }
}

variable "governed_policies" {
  type = map(object({
    service_accounts = map(list(string))
  }))
  description = "Exact namespace and ServiceAccount operation roles for governed object rows and execution grants."
}

variable "ducklake_root" {
  type        = string
  default     = "ducklake/"
  description = "Physical key root accepted by the governed DuckLake S3 surface. This is placement only, never authority."

  validation {
    condition     = can(regex("^[^/]+(/[^/]+)*/$", var.ducklake_root)) && !strcontains(var.ducklake_root, "..")
    error_message = "ducklake_root must be a relative, non-traversing key prefix ending in /."
  }
}

variable "ducklake_max_put_bytes" {
  type        = number
  default     = 100663296
  description = "Maximum governed DuckLake single-PUT body size (96 MiB; headroom over nimmly's 32 MB target file size)."
}

variable "lake_operation_timeout" {
  type        = string
  default     = "5m"
  description = "Overall deadline for one governed DuckLake S3 operation."
}

variable "lake_io_idle_timeout" {
  type        = string
  default     = "30s"
  description = "Rolling read/write deadline while streaming governed DuckLake bodies."
}

variable "lake_max_concurrency" {
  type        = number
  default     = 4
  description = "Per-replica cap for concurrent governed DuckLake S3 operations."
}

# --- Resources -------------------------------------------------------------
variable "resources" {
  type = object({
    requests = object({ cpu = string, memory = string })
    limits   = object({ cpu = string, memory = string })
  })
  default = {
    requests = { cpu = "50m", memory = "64Mi" }
    limits   = { cpu = "200m", memory = "128Mi" }
  }
  description = "Container resource requests/limits."
}
