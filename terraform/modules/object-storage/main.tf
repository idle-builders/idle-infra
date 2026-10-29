# =============================================================================
# modules/object-storage/main.tf
# =============================================================================
#
# Per-tenant application object storage on Infomaniak Object Storage (Swift /
# S3 gateway). Provisions one bucket per app plus an S3 credential, and injects
# an `object-storage` secret into the tenant namespace — mirroring how the
# tenant module ships `database-url`.
#
# ISOLATION (read this before assuming hard boundaries):
#   Infomaniak Swift has NO per-bucket credential scoping and no bucket policies.
#   An EC2/S3 credential is PROJECT-scoped — it can reach every container in the
#   project (other tenants' buckets, the infra-backups bucket, and the
#   infra-tfstate bucket). So cross-tenant isolation here is app-layer +
#   blast-radius reduction, NOT a cryptographic boundary:
#     - each app gets its OWN credential (revoke/rotate one without the others);
#     - each app is told only its own bucket and must use only that;
#     - within-app isolation stays app-layer (e.g. ambrovia's {projectId}/{sourceId}
#       prefixes, nimmly's per-workspace prefixes).
#   The TF state is AES-256-GCM encrypted, so a leaked app credential yields
#   ciphertext only. Hard isolation = a separate OpenStack project per tenant —
#   deliberately DEFERRED until a customer/contract requires provable isolation.
# =============================================================================

terraform {
  required_providers {
    openstack = {
      source  = "terraform-provider-openstack/openstack"
      version = "~> 3.0"
    }
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 2.35"
    }
  }
}

locals {
  bucket_name = "${var.tenant_name}-files"
}

resource "openstack_objectstorage_container_v1" "bucket" {
  name   = local.bucket_name
  region = var.region

  metadata = {
    tenant     = var.tenant_name
    managed-by = "terraform"
  }
}

# Project-scoped S3 credential for this app (see ISOLATION note above).
# Rotate by tainting this resource and re-applying (regenerates access/secret
# and refreshes the secret below).
resource "openstack_identity_ec2_credential_v3" "s3" {}

resource "kubernetes_secret" "object_storage" {
  metadata {
    name      = "object-storage"
    namespace = var.tenant_name

    labels = {
      "platform.infra/managed-by" = "terraform"
    }
  }

  data = {
    S3_ENDPOINT          = var.s3_endpoint
    S3_REGION            = var.s3_region
    S3_BUCKET            = openstack_objectstorage_container_v1.bucket.name
    S3_ACCESS_KEY_ID     = openstack_identity_ec2_credential_v3.s3.access
    S3_SECRET_ACCESS_KEY = openstack_identity_ec2_credential_v3.s3.secret
  }

  type = "Opaque"
}
