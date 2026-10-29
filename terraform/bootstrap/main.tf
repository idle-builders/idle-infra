# =============================================================================
# terraform/bootstrap/main.tf
# =============================================================================
#
# One-time bootstrap: documents the S3 bucket creation for Terraform state.
#
# STATUS: The infra-tfstate bucket already exists at s3.pub2.infomaniak.cloud
# (created via `openstack container create infra-tfstate`). This module is
# kept for documentation and recovery purposes only.
#
# To recreate the bucket if lost:
#   cd terraform/bootstrap
#   terraform init
#   source /path/to/openrc.sh   # Set OS_* environment variables
#   terraform apply
#
# After creation, configure terraform/main/ backend to use this bucket and
# run `terraform init` there.
# =============================================================================

terraform {
  required_version = ">= 1.6"

  # Local state — intentional. This module creates the remote state bucket
  # so it cannot itself use remote state. Keep this local state file safe.
  required_providers {
    openstack = {
      source  = "terraform-provider-openstack/openstack"
      version = "~> 3.0"
    }
  }
}

provider "openstack" {
  # Reads OS_AUTH_URL, OS_PROJECT_NAME, OS_USERNAME, OS_PASSWORD, etc.
  # from environment variables set by sourcing the Infomaniak OpenRC file.
  # Auth URL: https://api.pub1.infomaniak.cloud/identity/v3
  # Region: dc4-a
}

# S3-compatible object storage container for Terraform state.
# Infomaniak uses OpenStack Swift with an S3 middleware layer.
resource "openstack_objectstorage_container_v1" "tfstate" {
  name   = var.bucket_name
  region = var.region

  metadata = {
    purpose     = "terraform-state"
    managed-by  = "terraform"
    environment = "infra"
  }

  # versioning_legacy requires a separate container to store non-current object
  # versions. NOTE: verify this attribute is supported by your OpenStack Swift
  # deployment — Swift versioning support varies by provider configuration.
  versioning_legacy {
    type     = "history"
    location = openstack_objectstorage_container_v1.tfstate_versions.name
  }
}

# Container that holds previous versions of state objects.
# Referenced by the versioning_legacy block on the tfstate container above.
resource "openstack_objectstorage_container_v1" "tfstate_versions" {
  name   = "${var.bucket_name}-versions"
  region = var.region

  metadata = {
    purpose     = "terraform-state-versions"
    managed-by  = "terraform"
    environment = "infra"
  }
}
