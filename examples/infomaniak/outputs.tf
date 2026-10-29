output "cluster_id" { value = module.cluster.cluster_id }
output "node_pool_id" { value = module.cluster.node_pool_id }
output "tunnel_id" { value = module.edge.tunnel_id }
output "tenants" { value = { for name, tenant in module.tenant : name => { namespace = tenant.namespace, schema = tenant.pg_schema_name, deployer = tenant.deployer_service_account } } }
