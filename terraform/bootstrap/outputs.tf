output "bucket_name" {
  value       = openstack_objectstorage_container_v1.tfstate.name
  description = "Name of the Terraform state S3 bucket."
}

output "s3_endpoint" {
  value       = "https://s3.pub2.infomaniak.cloud"
  description = "S3-compatible endpoint for Infomaniak object storage."
}

output "backend_config_hint" {
  value       = <<-EOT
    Configure terraform/main/ backend with:
      bucket   = "${openstack_objectstorage_container_v1.tfstate.name}"
      key      = "main/terraform.tfstate"
      endpoint = "https://s3.pub2.infomaniak.cloud"
    Then run: terraform init -reconfigure
  EOT
  description = "Hint for configuring the S3 backend in terraform/main/."
}
