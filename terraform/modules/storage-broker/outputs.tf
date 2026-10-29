output "service_dns" {
  value       = "${kubernetes_service_v1.broker.metadata[0].name}.${var.namespace}.svc.cluster.local"
  description = "In-cluster DNS name apps call for presigned URLs (e.g. http://storage-broker.infra.svc.cluster.local/v1/presign)."
}

output "service_name" {
  value       = kubernetes_service_v1.broker.metadata[0].name
  description = "Name of the broker ClusterIP service."
}

output "token_audience" {
  value       = var.token_audience
  description = "Audience apps must request on their projected ServiceAccount token."
}
