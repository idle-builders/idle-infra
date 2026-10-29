output "namespace" {
  value = kubernetes_namespace.keda.metadata[0].name
}
