variable "cloudflare_account_id" {
  type        = string
  sensitive   = true
  default     = ""
  description = "Cloudflare account ID. Used to enumerate/scope zones for DNS management."
}

variable "namespace" {
  type    = string
  default = "infra"
}

variable "tunnel_name" {
  type    = string
  default = "infra-cluster"
}

variable "node_selector" {
  type    = map(string)
  default = {}
}
