output "cluster_id" {
  value = infomaniak_kaas.cluster.id
}

output "node_pool_id" {
  value = infomaniak_kaas_instance_pool.workers_disk50.id
}
