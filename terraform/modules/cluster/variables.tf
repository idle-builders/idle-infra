variable "infomaniak_project_id" {
  type        = number
  description = "Infomaniak project ID."
}

variable "infomaniak_public_cloud_id" {
  type        = number
  description = "Infomaniak public cloud ID."
}

variable "kubernetes_version" {
  type        = string
  default     = "1.35"
  description = "Kubernetes version for the managed cluster."
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

variable "cluster_name" {
  type    = string
  default = "infra"
}

variable "cluster_pack" {
  type    = string
  default = "shared"
}

variable "region" {
  type    = string
  default = "dc4-a"
}

variable "node_pool_name" {
  type    = string
  default = "workers"
}

variable "availability_zone" {
  type    = string
  default = "az-1"
}

variable "node_labels" {
  type    = map(string)
  default = {}
}
