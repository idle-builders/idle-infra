# =============================================================================
# modules/tenant/main.tf
# =============================================================================
#
# Per-tenant isolation scaffolding. Provisions:
#   - Kubernetes namespace with labels
#   - Network policies (default-deny + explicit allowlist)
#   - ResourceQuota
#   - RBAC: ServiceAccount + Role + RoleBinding for CI/CD deployer
#   - PostgreSQL role and schema with isolation grants
#   - K8s secrets: database-url, ghcr-pull
#
# Called once per tenant in terraform/main/main.tf.
# =============================================================================

terraform {
  required_providers {
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 2.35"
    }
    postgresql = {
      source  = "cyrilgdn/postgresql"
      version = "~> 1.25"
    }
  }
}

# =============================================================================
# Kubernetes namespace
# =============================================================================

resource "kubernetes_namespace" "tenant" {
  metadata {
    name = var.tenant_name

    labels = {
      "kubernetes.io/metadata.name"        = var.tenant_name
      "platform.infra/tenant"              = "true"
      "pod-security.kubernetes.io/enforce" = "restricted"
      "pod-security.kubernetes.io/warn"    = "restricted"
    }

    annotations = {
      # Ingress hosts this tenant may claim (exact or subdomain of these).
      # Enforced fail-closed by the tenant-ingress-hostname admission policy.
      "platform.infra/allowed-hosts" = join(",", var.allowed_ingress_domains)
    }
  }
}

resource "kubernetes_default_service_account" "tenant" {
  metadata {
    namespace = kubernetes_namespace.tenant.metadata[0].name
  }

  automount_service_account_token = false
}

# =============================================================================
# Network policies
# =============================================================================

# Default deny all ingress and egress
resource "kubernetes_network_policy" "default_deny" {
  metadata {
    name      = "default-deny"
    namespace = kubernetes_namespace.tenant.metadata[0].name
  }

  spec {
    pod_selector {}

    policy_types = ["Ingress", "Egress"]
  }
}

# Allow DNS resolution via kube-system
resource "kubernetes_network_policy" "allow_dns" {
  metadata {
    name      = "allow-dns"
    namespace = kubernetes_namespace.tenant.metadata[0].name
  }

  spec {
    pod_selector {}

    policy_types = ["Egress"]

    egress {
      to {
        namespace_selector {
          match_labels = {
            "kubernetes.io/metadata.name" = "kube-system"
          }
        }
      }

      ports {
        protocol = "UDP"
        port     = "53"
      }

      ports {
        protocol = "TCP"
        port     = "53"
      }
    }
  }
}

# Allow egress to PostgreSQL in infra namespace — pod-scoped to the postgresql pod only.
# Replaces the former allow-infra policy which granted overly broad namespace-level access.
resource "kubernetes_network_policy" "allow_postgres" {
  metadata {
    name      = "allow-postgres"
    namespace = kubernetes_namespace.tenant.metadata[0].name
  }

  spec {
    pod_selector {
      dynamic "match_expressions" {
        for_each = var.baseline_egress_excluded_label_keys
        content {
          key      = match_expressions.value
          operator = "DoesNotExist"
        }
      }
    }

    policy_types = ["Egress"]

    egress {
      to {
        namespace_selector {
          match_labels = {
            "kubernetes.io/metadata.name" = "infra"
          }
        }

        pod_selector {
          match_labels = {
            "app.kubernetes.io/name" = "postgresql"
          }
        }
      }

      ports {
        protocol = "TCP"
        port     = "5432"
      }
    }
  }
}

# Allow egress to Zitadel in infra namespace — pod-scoped to the zitadel pod only.
# Tenant apps reach Zitadel directly for token validation, JWKS, and userinfo.
resource "kubernetes_network_policy" "allow_zitadel" {
  metadata {
    name      = "allow-zitadel"
    namespace = kubernetes_namespace.tenant.metadata[0].name
  }

  spec {
    pod_selector {
      dynamic "match_expressions" {
        for_each = var.baseline_egress_excluded_label_keys
        content {
          key      = match_expressions.value
          operator = "DoesNotExist"
        }
      }
    }

    policy_types = ["Egress"]

    egress {
      to {
        namespace_selector {
          match_labels = {
            "kubernetes.io/metadata.name" = "infra"
          }
        }

        pod_selector {
          match_labels = {
            "app.kubernetes.io/name" = "zitadel"
          }
        }
      }

      ports {
        protocol = "TCP"
        port     = "8080"
      }
    }
  }
}

# Allow egress to the storage-broker in infra namespace — pod-scoped to the
# broker pod only. Apps request short-TTL presigned S3 URLs here; they hold no
# S3 credential of their own. See modules/storage-broker.
resource "kubernetes_network_policy" "allow_storage_broker" {
  metadata {
    name      = "allow-storage-broker"
    namespace = kubernetes_namespace.tenant.metadata[0].name
  }

  spec {
    pod_selector {
      dynamic "match_expressions" {
        for_each = var.baseline_egress_excluded_label_keys
        content {
          key      = match_expressions.value
          operator = "DoesNotExist"
        }
      }
    }

    policy_types = ["Egress"]

    egress {
      to {
        namespace_selector {
          match_labels = {
            "kubernetes.io/metadata.name" = "infra"
          }
        }

        pod_selector {
          match_labels = {
            "app.kubernetes.io/name" = "storage-broker"
          }
        }
      }

      ports {
        protocol = "TCP"
        port     = "8080"
      }

      # lake-s3: the broker's S3-compatible proxy that DuckLake publication reads and writes through.
      ports {
        protocol = "TCP"
        port     = "8081"
      }
    }
  }
}

# Application telemetry is opt-in per tenant. The receiver remains internal,
# pod-scoped, and HTTP-only; telemetry failure never affects application work.
resource "kubernetes_network_policy" "allow_observability_otlp" {
  count = var.allow_observability_otlp_egress ? 1 : 0

  metadata {
    name      = "allow-observability-otlp"
    namespace = kubernetes_namespace.tenant.metadata[0].name
  }

  spec {
    pod_selector {}
    policy_types = ["Egress"]

    egress {
      to {
        namespace_selector {
          match_labels = {
            "kubernetes.io/metadata.name" = "observability"
          }
        }

        pod_selector {
          match_labels = {
            "app.kubernetes.io/name" = "observability-collector"
          }
        }
      }

      ports {
        protocol = "TCP"
        port     = "4318"
      }
    }
  }
}

# Accepted risk: unrestricted HTTPS egress. Tenant apps need outbound HTTPS for
# external APIs, LLM providers, payment processors. Restricting to specific CIDRs
# is not feasible (CDNs use dynamic IPs). Mitigated by port 443 only + RFC1918 exclusion.
resource "kubernetes_network_policy" "allow_internet_egress" {
  metadata {
    name      = "allow-internet-egress"
    namespace = kubernetes_namespace.tenant.metadata[0].name
  }

  spec {
    pod_selector {
      dynamic "match_expressions" {
        for_each = var.baseline_egress_excluded_label_keys
        content {
          key      = match_expressions.value
          operator = "DoesNotExist"
        }
      }
    }

    policy_types = ["Egress"]

    egress {
      to {
        ip_block {
          cidr = "0.0.0.0/0"
          except = [
            "10.0.0.0/8",
            "172.16.0.0/12",
            "192.168.0.0/16",
          ]
        }
      }

      ports {
        protocol = "TCP"
        port     = "443"
      }
    }
  }
}

# Allow ingress from ingress-nginx controller pods in the infra namespace.
resource "kubernetes_network_policy" "allow_ingress_controller" {
  metadata {
    name      = "allow-ingress-controller"
    namespace = kubernetes_namespace.tenant.metadata[0].name
  }

  spec {
    pod_selector {}

    policy_types = ["Ingress"]

    ingress {
      from {
        namespace_selector {
          match_labels = {
            "kubernetes.io/metadata.name" = "infra"
          }
        }

        pod_selector {
          match_labels = {
            "app.kubernetes.io/name" = "ingress-nginx"
          }
        }
      }
    }
  }
}

# =============================================================================
# ResourceQuota
# =============================================================================

resource "kubernetes_resource_quota" "tenant" {
  metadata {
    name      = "tenant-quota"
    namespace = kubernetes_namespace.tenant.metadata[0].name
  }

  spec {
    hard = {
      "requests.cpu"           = var.cpu_request
      "requests.memory"        = var.memory_request
      "limits.cpu"             = var.cpu_limit
      "limits.memory"          = var.memory_limit
      "pods"                   = "20"
      "services"               = "10"
      "persistentvolumeclaims" = "5"
    }
  }
}

resource "kubernetes_limit_range" "tenant" {
  metadata {
    name      = "tenant-defaults"
    namespace = kubernetes_namespace.tenant.metadata[0].name
  }

  spec {
    limit {
      type = "Container"

      default = {
        cpu    = "500m"
        memory = "512Mi"
      }

      default_request = {
        cpu    = "100m"
        memory = "128Mi"
      }

      max = {
        cpu    = "2"
        memory = "4Gi"
      }

      min = {
        cpu    = "10m"
        memory = "16Mi"
      }
    }
  }
}

# =============================================================================
# RBAC — deployer service account for CI/CD
# =============================================================================

resource "kubernetes_service_account" "deployer" {
  metadata {
    name      = "deployer"
    namespace = kubernetes_namespace.tenant.metadata[0].name

    labels = {
      "platform.infra/managed-by" = "terraform"
    }
  }

  automount_service_account_token = false
}

resource "kubernetes_role" "deployer" {
  metadata {
    name      = "deployer"
    namespace = kubernetes_namespace.tenant.metadata[0].name
  }

  rule {
    api_groups = ["apps"]
    resources  = ["deployments"]
    verbs      = ["get", "list", "watch", "create", "update", "patch"]
  }

  # Read-only access to ReplicaSets. `helm upgrade --wait` (and `kubectl rollout status`)
  # tracks a Deployment's progress by listing the ReplicaSets it owns; without this the
  # deploy fails with "replicasets.apps is forbidden". Read-only — Deployments own RS lifecycle.
  # Shared tenant-module change: applies to every tenant's deploy credential.
  rule {
    api_groups = ["apps"]
    resources  = ["replicasets"]
    verbs      = ["get", "list", "watch"]
  }

  rule {
    api_groups = [""]
    resources  = ["services", "configmaps"]
    verbs      = ["get", "list", "watch", "create", "update", "patch"]
  }

  # Helm release history is stored in ConfigMaps for tenant app deploys
  # (HELM_DRIVER=configmap). Helm deletes/recreates release ConfigMaps during
  # some failed-upgrade recovery paths, so the deployer needs ConfigMap delete
  # without gaining Secrets access.
  rule {
    api_groups = [""]
    resources  = ["configmaps"]
    verbs      = ["delete"]
  }

  rule {
    api_groups = ["networking.k8s.io"]
    resources  = ["ingresses"]
    verbs      = ["get", "list", "watch", "create", "update", "patch"]
  }

  # Tenant charts may protect application replicas with a PodDisruptionBudget.
  # @lore Helm checks for a desired PDB before creating it, so even a first
  # install needs `get`; full lifecycle verbs keep later upgrades/removal from
  # failing while remaining limited to PDBs in the tenant's own namespace.
  rule {
    api_groups = ["policy"]
    resources  = ["poddisruptionbudgets"]
    verbs      = ["get", "list", "watch", "create", "update", "patch", "delete"]
  }

  # Read-only access to pods for deployment status checks and log streaming
  rule {
    api_groups = [""]
    resources  = ["pods", "pods/log"]
    verbs      = ["get", "list", "watch"]
  }

  # Helm pre-install/pre-upgrade migration hook Job (chart/templates/migrate-job.yaml).
  # create: Helm creates the hook Job.
  # delete: required by hook-delete-policy "before-hook-creation" — Helm deletes
  #         the prior hook Job before re-creating it on the next upgrade.
  # get/list/watch: `helm upgrade --wait` polls the Job to completion.
  #
  # ⚠️ This rule is added to the deployer Role for ALL tenants (ambrovia, hyperidle,
  # nimmly, any future tenant), not just nimmly — it lives in the shared tenant module.
  # Blast radius: every tenant's deploy credential gains batch/jobs (get/list/watch/
  # create/delete). No secret/exec access added. Any tenant adopting Helm-hook
  # migrations benefits automatically.
  rule {
    api_groups = ["batch"]
    resources  = ["jobs"]
    verbs      = ["get", "list", "watch", "create", "delete"]
  }

  # Tenant apps may ship scheduled worker workloads as CronJobs.
  # This enables Helm-managed CronJobs without granting broader cluster access.
  rule {
    api_groups = ["batch"]
    resources  = ["cronjobs"]
    verbs      = ["get", "list", "watch", "create", "update", "patch", "delete"]
  }

  # Intentionally excluded: secrets (managed by Terraform). Delete remains
  # limited to ConfigMaps for Helm release recovery and batch workloads above.
}

resource "kubernetes_role_binding" "deployer" {
  metadata {
    name      = "deployer"
    namespace = kubernetes_namespace.tenant.metadata[0].name
  }

  role_ref {
    api_group = "rbac.authorization.k8s.io"
    kind      = "Role"
    name      = kubernetes_role.deployer.metadata[0].name
  }

  subject {
    kind      = "ServiceAccount"
    name      = kubernetes_service_account.deployer.metadata[0].name
    namespace = kubernetes_namespace.tenant.metadata[0].name
  }
}

# =============================================================================
# PostgreSQL: role, schema, and grants
# =============================================================================

resource "postgresql_role" "tenant" {
  name     = var.tenant_name
  login    = true
  password = var.tenant_db_password

  lifecycle {
    ignore_changes = [password]
  }
}

resource "postgresql_schema" "tenant" {
  name  = var.tenant_name
  owner = postgresql_role.tenant.name

  depends_on = [postgresql_role.tenant]
}

# Grant the tenant role full access to its own schema
resource "postgresql_grant" "tenant_schema" {
  database    = var.database_name
  role        = postgresql_role.tenant.name
  schema      = postgresql_schema.tenant.name
  object_type = "schema"
  privileges  = ["CREATE", "USAGE"]

  depends_on = [postgresql_schema.tenant]
}

# =============================================================================
# Kubernetes secrets
# =============================================================================

# Database connection URL with search_path set to the tenant's schema.
# HyperIdle's Drizzle uses pgTable() (unqualified names) — search_path resolves
# all table references to the tenant schema. Zero app code changes required.
resource "kubernetes_secret" "database_url" {
  metadata {
    name      = "database-url"
    namespace = kubernetes_namespace.tenant.metadata[0].name
  }

  data = {
    DATABASE_URL = "postgresql://${postgresql_role.tenant.name}:${var.tenant_db_password}@${var.pg_host}:${var.pg_port}/${var.database_name}?sslmode=verify-ca&sslrootcert=/etc/ssl/pg/ca.crt&options=-csearch_path%3D${var.tenant_name}"
  }

  lifecycle {
    ignore_changes = [data]
  }

  depends_on = [postgresql_schema.tenant]
}

# PostgreSQL CA certificate for SSL verify-ca connections.
# Tenant deployments must mount this secret at /etc/ssl/pg/ to match
# the sslrootcert path in DATABASE_URL.
resource "kubernetes_secret" "pg_ca_cert" {
  metadata {
    name      = "pg-ca-cert"
    namespace = kubernetes_namespace.tenant.metadata[0].name
  }

  data = {
    "ca.crt" = var.pg_ca_cert
  }
}

# GHCR image pull secret for pulling private container images
resource "kubernetes_secret" "ghcr_pull" {
  metadata {
    name      = "ghcr-pull"
    namespace = kubernetes_namespace.tenant.metadata[0].name
  }

  type = "kubernetes.io/dockerconfigjson"

  data = {
    ".dockerconfigjson" = jsonencode({
      auths = {
        "ghcr.io" = {
          auth = base64encode("${var.ghcr_username}:${var.ghcr_token}")
        }
      }
    })
  }
}
