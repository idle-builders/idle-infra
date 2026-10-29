# =============================================================================
# terraform/main/main.tf
# =============================================================================
#
# Root module: provisions the shared infrastructure platform.
#
# Apply ordering on first run:
#   1. terraform apply -target=infomaniak_kaas.cluster -target=infomaniak_kaas_instance_pool.workers_disk50
#      Creates the cluster and autoscaling worker pool. Wait for nodes to be Ready.
#   2. Download kubeconfig from Infomaniak console > ../../cert/kubeconfig
#      (Do NOT use terraform output — kubeconfig is intentionally not in state.)
#   3. terraform apply -target=helm_release.postgresql
#      Wait for postgresql pod to be Running.
#   4. kubectl port-forward -n infra svc/postgresql-primary 5432:5432 &
#   5. terraform apply   (full apply — creates schemas, roles, tenants, and ingress)
#
# =============================================================================

# =============================================================================
# K8s cluster (Infomaniak Managed Kubernetes — shared/free control plane)
# =============================================================================
resource "infomaniak_kaas" "cluster" {
  public_cloud_id         = var.infomaniak_public_cloud_id
  public_cloud_project_id = var.infomaniak_project_id

  name               = var.cluster_name
  pack_name          = var.cluster_pack
  region             = var.region
  kubernetes_version = var.kubernetes_version
}

# =============================================================================
# Node pool — three-node baseline, autoscaling to four
# =============================================================================

# This is the sole worker pool. Pool 4911 and its three 20 GB nodes were drained
# before this resource became the only declared worker capacity.
resource "infomaniak_kaas_instance_pool" "workers_disk50" {
  public_cloud_id         = var.infomaniak_public_cloud_id
  public_cloud_project_id = var.infomaniak_project_id
  kaas_id                 = infomaniak_kaas.cluster.id

  name              = var.node_pool_name
  flavor_name       = var.node_pool_flavor
  availability_zone = var.availability_zone
  min_instances     = var.node_pool_min_size
  max_instances     = var.node_pool_max_size

  labels = var.node_labels
}
