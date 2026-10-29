variable "tenant_name" {
  type        = string
  description = "Tenant name. Used for the bucket name (<tenant>-files) and the target namespace for the object-storage secret."
}

variable "region" {
  type        = string
  default     = "dc4-a"
  description = "Infomaniak OpenStack region for the Swift container."
}

variable "s3_endpoint" {
  type        = string
  default     = "https://s3.pub2.infomaniak.cloud"
  description = "S3-compatible endpoint for Infomaniak object storage (dc4 / pub2)."
}

variable "s3_region" {
  type        = string
  default     = "us-east-1"
  description = "S3 region string for SDK compatibility. Infomaniak's S3 gateway ignores it."
}
