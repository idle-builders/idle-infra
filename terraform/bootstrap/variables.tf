variable "bucket_name" {
  type        = string
  default     = "infra-tfstate"
  description = "Name of the OpenStack Swift container used for Terraform state storage."
}

variable "region" {
  type        = string
  default     = "dc4-a"
  description = "Infomaniak OpenStack region."
}
