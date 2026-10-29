variable "tenant_name" {
  type        = string
  description = "Tenant identifier. Used as namespace name, schema name, and PG role name. Must be lowercase alphanumeric."

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]*$", var.tenant_name))
    error_message = "Tenant name must start with a lowercase letter and contain only lowercase letters, digits, and hyphens."
  }
}

variable "tenant_db_password" {
  type        = string
  sensitive   = true
  description = "Password for the tenant's PostgreSQL role."
}

variable "pg_host" {
  type        = string
  description = "PostgreSQL hostname (cluster-internal DNS name)."
}

variable "pg_port" {
  type        = number
  default     = 5432
  description = "PostgreSQL port."
}

variable "database_name" {
  type        = string
  description = "PostgreSQL database name that contains the tenant's schema."
}

variable "ghcr_username" {
  type        = string
  description = "GitHub username or organisation for GHCR pull access."
}

variable "ghcr_token" {
  type        = string
  sensitive   = true
  description = "GitHub PAT with read:packages scope for GHCR pull access."
}

variable "pg_ca_cert" {
  type        = string
  sensitive   = true
  description = "PostgreSQL CA certificate (PEM) for SSL verification. Distributed to the tenant namespace as a K8s secret."
}

variable "cpu_request" {
  type        = string
  default     = "2"
  description = "Total CPU requests allowed in the tenant namespace."
}

variable "memory_request" {
  type        = string
  default     = "4Gi"
  description = "Total memory requests allowed in the tenant namespace."
}

variable "cpu_limit" {
  type        = string
  default     = "4"
  description = "Total CPU limits allowed in the tenant namespace."
}

variable "memory_limit" {
  type        = string
  default     = "8Gi"
  description = "Total memory limits allowed in the tenant namespace."
}

variable "allowed_ingress_domains" {
  type        = list(string)
  description = <<-EOT
    Domains this tenant may use as Ingress hosts. A host is permitted if it
    equals one of these or is a subdomain of it — e.g. "app.example.com" permits
    app.example.com, www.app.example.com, and beta.app.example.com. Written to the
    platform.infra/allowed-hosts namespace annotation and enforced (fail-closed)
    by the tenant-ingress-hostname ValidatingAdmissionPolicy. Must NOT include
    domains owned by infra/other tenants (e.g. nimmly.io hosts the IdP).
  EOT

  validation {
    condition     = length(var.allowed_ingress_domains) > 0
    error_message = "Each tenant must declare at least one allowed ingress domain (fail-closed policy)."
  }
}

variable "baseline_egress_excluded_label_keys" {
  type        = set(string)
  default     = []
  description = "Pod label keys excluded from the tenant-wide PostgreSQL, storage-broker, Zitadel, and public HTTPS egress policies."
}

variable "allow_observability_otlp_egress" {
  type        = bool
  default     = false
  description = "Allow this tenant namespace to send OTLP/HTTP to the shared node-local Collector."
}
