output "bucket_name" {
  value       = openstack_objectstorage_container_v1.telemetry.name
  description = "Object-storage container holding observability data."
}

output "otlp_grpc_endpoint" {
  value       = "observability-collector.${var.namespace}.svc.cluster.local:4317"
  description = "In-cluster OTLP/gRPC endpoint. The Service routes to the collector on the caller's node."
}

output "otlp_http_endpoint" {
  value       = "http://observability-collector.${var.namespace}.svc.cluster.local:4318"
  description = "In-cluster OTLP/HTTP endpoint. The Service routes to the collector on the caller's node."
}
