output "service_dns" {
  value       = "${kubernetes_service_v1.console.metadata[0].name}.${var.namespace}.svc.cluster.local"
  description = "In-cluster DNS name of the console service."
}

output "service_name" {
  value       = kubernetes_service_v1.console.metadata[0].name
  description = "Console service name."
}


