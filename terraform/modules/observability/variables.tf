variable "namespace" {
  type        = string
  default     = "observability"
  description = "Namespace for the node collectors and compaction CronJob."
}

variable "otlp_ingress_namespaces" {
  type        = set(string)
  default     = ["infra"]
  description = "Explicit namespaces allowed to send OTLP telemetry to the Collector service."

  validation {
    condition = length(var.otlp_ingress_namespaces) > 0 && alltrue([
      for namespace in var.otlp_ingress_namespaces : can(regex("^[a-z0-9]([a-z0-9-]*[a-z0-9])?$", namespace))
    ])
    error_message = "otlp_ingress_namespaces must contain valid Kubernetes namespace names."
  }
}

variable "collector_image" {
  type        = string
  description = "Digest-pinned custom OpenTelemetry Collector image."

  validation {
    condition     = can(regex("@sha256:[0-9a-f]{64}$", var.collector_image))
    error_message = "collector_image must use an immutable sha256 digest."
  }
}

variable "compactor_image" {
  type        = string
  description = "Digest-pinned telemetry-data compactor image."

  validation {
    condition     = can(regex("@sha256:[0-9a-f]{64}$", var.compactor_image))
    error_message = "compactor_image must use an immutable sha256 digest."
  }
}

variable "ghcr_pull_secret_name" {
  type        = string
  default     = "ghcr-pull"
  description = "Private GHCR image pull secret in var.namespace."
}

variable "collector_config" {
  type        = string
  description = "Complete OpenTelemetry Collector configuration."
}

variable "bucket_name" {
  type        = string
  default     = "infra-observability"
  description = "Infomaniak object-storage container holding raw and curated telemetry."
}

variable "object_storage_region" {
  type        = string
  default     = "dc4-a"
  description = "OpenStack region for the Swift container."
}

variable "s3_endpoint" {
  type        = string
  default     = "https://s3.pub2.infomaniak.cloud"
  description = "Infomaniak S3-compatible endpoint."
}

variable "s3_region" {
  type        = string
  default     = "us-east-1"
  description = "S3 SDK region compatibility value."
}

variable "compaction_schedule" {
  type        = string
  default     = "*/5 * * * *"
  description = "Cron schedule for converting sealed raw minute objects to Parquet."
}

variable "raw_retention_seconds" {
  type        = number
  default     = 7200
  description = "Age after which a raw object may be deleted, but only after its Parquet output is committed."
}

variable "collector_resources" {
  type = object({
    requests = object({ cpu = string, memory = string })
    limits   = object({ cpu = string, memory = string })
  })
  default = {
    requests = { cpu = "25m", memory = "96Mi" }
    limits   = { cpu = "200m", memory = "192Mi" }
  }
  description = "Per-node collector resource requests and limits."
}

variable "compactor_resources" {
  type = object({
    requests = object({ cpu = string, memory = string })
    limits   = object({ cpu = string, memory = string })
  })
  default = {
    requests = { cpu = "25m", memory = "64Mi" }
    limits   = { cpu = "500m", memory = "512Mi" }
  }
  description = "Short-lived compactor job resource requests and limits."
}

variable "apiserver_cidr" {
  type = string
}

variable "apiserver_port" {
  type = number
}

variable "application_telemetry_sources" {
  description = "Exact application identities admitted to bounded execution summaries. Empty disables application summaries; OTLP network admission remains separately configured. Set before upgrading telemetry-data images."
  type = list(object({
    k8s_namespace     = string
    service_namespace = string
    service_name      = string
    workload          = optional(string)
    cronjob           = optional(string)
  }))
  default = []

  validation {
    condition = alltrue([for source in var.application_telemetry_sources :
      (source.workload != null) != (source.cronjob != null)
    ])
    error_message = "Every application telemetry source needs exactly one workload or cronjob selector."
  }
}
