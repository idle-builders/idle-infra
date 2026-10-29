
variable "apiserver_cidr" {
  type        = string
  description = "Post-DNAT Kubernetes API endpoint CIDR for network policy."
}

variable "apiserver_port" {
  type        = number
  description = "Post-DNAT Kubernetes API endpoint port."
}

variable "helm_values" {
  type        = string
  description = "KEDA values document; declares the watched tenant namespace."
}
