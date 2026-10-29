output "bucket_name" {
  value       = openstack_objectstorage_container_v1.bucket.name
  description = "Name of the tenant's object-storage bucket."
}

output "s3_endpoint" {
  value       = var.s3_endpoint
  description = "S3-compatible endpoint for Infomaniak object storage."
}

output "secret_name" {
  value       = kubernetes_secret.object_storage.metadata[0].name
  description = "Name of the object-storage secret injected into the tenant namespace."
}
