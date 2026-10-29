# =============================================================================
# providers.tf — Provider configuration
# =============================================================================

# Infomaniak provider — K8s cluster, node pools, DNS
# Auth: INFOMANIAK_TOKEN environment variable (OAuth2 bearer token)
provider "infomaniak" {}

# OpenStack provider — networking (floating IPs)
# Auth: OS_* environment variables from sourcing the Infomaniak OpenRC file
provider "openstack" {
  region = var.region
}

# Cloudflare provider — DNS records (v5.x; cloudflare_dns_record resource)
# Auth: var.cloudflare_api_token (TF_VAR_cloudflare_api_token from Keychain/CI).
# Token scope: Zone:Read (all) + DNS:Edit (managed zones).
provider "cloudflare" {
  api_token = var.cloudflare_api_token
}

# Kubernetes and Helm providers use the kubeconfig file.
# On first apply, the cluster doesn't exist yet, so:
#   1. terraform apply -target=module.cluster -target=module.cluster
#   2. Save kubeconfig: terraform output -raw kubeconfig > ../../cert/kubeconfig
#   3. terraform apply (full)
provider "kubernetes" {
  config_path = var.kubeconfig_path
}

provider "helm" {
  kubernetes {
    config_path = var.kubeconfig_path
  }
}

# PostgreSQL provider — schema and role management inside the self-hosted PG cluster.
#
# IMPORTANT: Connection to in-cluster PostgreSQL.
#
# This provider connects to PostgreSQL running inside the Kubernetes cluster.
# For local `terraform apply`, you MUST run kubectl port-forward first:
#
#   kubectl port-forward -n infra svc/postgresql-primary 5432:5432 &
#
# Then apply runs against localhost:5432.
#
# For GitHub Actions, the workflow runs port-forward as a background step
# before running terraform apply. See .github/workflows/terraform-apply.yml.
#
# First apply ordering:
#   The PostgreSQL Helm release (module.platform) must be applied
#   before this provider can connect. On initial setup, use:
#     terraform apply -target=module.platform
#   Wait for the pod to be ready, start port-forward, then run full apply.
#
provider "postgresql" {
  host     = "127.0.0.1"
  port     = var.postgresql_admin_port
  username = "postgres"
  password = var.pg_admin_password
  sslmode  = "disable" # Port-forward connections via 127.0.0.1 use kubectl tunnel encryption; in-cluster connections enforce SSL via hostssl pg_hba rules
}
