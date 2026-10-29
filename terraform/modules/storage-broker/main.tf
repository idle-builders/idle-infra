# =============================================================================
# modules/storage-broker/main.tf
# =============================================================================
#
# Credential-mediating governed object broker. It holds the ONLY S3 credential
# and owns authorization rows with two isolation levels:
#
#   - TENANT scope    — derived from the verified Kubernetes namespace.
#   - WORKSPACE scope — stored on the governed object/grant row and matched
#                       exactly; null workspace means tenant-wide.
#
# The legacy v1 prefix routes remain during migration. V2 callers use opaque
# object IDs and bounded execution grants; physical keys never carry authority.
# See docs/concepts/governed-object-storage.md.
#
# Runs in the infra namespace (PSA baseline-enforce / restricted-warn). The pod
# is restricted-compliant. DNS and K8s API egress come from the infra-wide
# allow-dns / allow-apiserver-egress policies; this module adds only the
# broker-specific egress (S3 + Zitadel on 443) and ingress (from tenants).
# PostgreSQL egress/ingress is declared at the shared database boundary.
# =============================================================================

terraform {
  required_providers {
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 2.35"
    }
  }
}

locals {
  name = "storage-broker"
  labels = {
    "app.kubernetes.io/name"       = "storage-broker"
    "app.kubernetes.io/component"  = "storage"
    "app.kubernetes.io/part-of"    = "platform-infra"
    "app.kubernetes.io/managed-by" = "opentofu"
  }
  selector_labels = {
    "app.kubernetes.io/name" = "storage-broker"
  }

  policy_json = jsonencode({
    defaultTTLSeconds = var.default_ttl_seconds
    namespaces = {
      for ns, p in var.tenant_policies : ns => {
        bucket           = p.bucket
        ops              = p.ops
        requireUserToken = p.require_user_token
        prefixes         = p.prefixes
      }
    }
  })

  governed_policy_json = jsonencode({
    namespaces = {
      for ns, p in var.governed_policies : ns => {
        serviceAccounts = p.service_accounts
      }
    }
  })
}

# --- Identity: SA + RBAC to call TokenReview -------------------------------
resource "kubernetes_service_account" "broker" {
  metadata {
    name      = local.name
    namespace = var.namespace
    labels    = local.labels
  }
}

resource "kubernetes_cluster_role" "broker_tokenreview" {
  metadata {
    name   = "storage-broker-tokenreview"
    labels = local.labels
  }

  rule {
    api_groups = ["authentication.k8s.io"]
    resources  = ["tokenreviews"]
    verbs      = ["create"]
  }
}

resource "kubernetes_cluster_role_binding" "broker_tokenreview" {
  metadata {
    name   = "storage-broker-tokenreview"
    labels = local.labels
  }

  role_ref {
    api_group = "rbac.authorization.k8s.io"
    kind      = "ClusterRole"
    name      = kubernetes_cluster_role.broker_tokenreview.metadata[0].name
  }

  subject {
    kind      = "ServiceAccount"
    name      = kubernetes_service_account.broker.metadata[0].name
    namespace = var.namespace
  }
}

# --- The S3 credential (lives only here) -----------------------------------
resource "kubernetes_secret" "broker_s3" {
  metadata {
    name      = "storage-broker-s3"
    namespace = var.namespace
    labels    = local.labels
  }

  data = {
    S3_ACCESS_KEY_ID     = var.s3_access_key_id
    S3_SECRET_ACCESS_KEY = var.s3_secret_access_key
  }

  type = "Opaque"
}

resource "kubernetes_secret" "broker_database" {
  metadata {
    name      = "storage-broker-database"
    namespace = var.namespace
    labels    = local.labels
  }

  data = {
    DATABASE_URL = var.database_url
  }

  type = "Opaque"
}

resource "kubernetes_secret" "broker_pg_ca" {
  metadata {
    name      = "storage-broker-pg-ca"
    namespace = var.namespace
    labels    = local.labels
  }

  data = {
    "ca.crt" = var.pg_ca_cert
  }

  type = "Opaque"
}

# --- Authorization policy (mounted file) -----------------------------------
resource "kubernetes_config_map" "broker_policy" {
  metadata {
    name      = "storage-broker-policy"
    namespace = var.namespace
    labels    = local.labels
  }

  data = {
    "policy.json"          = local.policy_json
    "governed-policy.json" = local.governed_policy_json
  }
}

# --- Deployment ------------------------------------------------------------
resource "kubernetes_deployment_v1" "broker" {
  metadata {
    name      = local.name
    namespace = var.namespace
    labels    = local.labels
  }

  # Egress + RBAC must exist before pods start so the first TokenReview / JWKS
  # fetch is permitted.
  depends_on = [
    kubernetes_network_policy.broker_egress,
    kubernetes_cluster_role_binding.broker_tokenreview,
  ]

  spec {
    replicas = var.replicas

    selector {
      match_labels = local.selector_labels
    }

    template {
      metadata {
        labels = local.labels
        # The broker loads its policy ONCE at startup (no hot-reload). Hash the
        # policy into the pod template so any tenant_policies change rolls the
        # Deployment automatically — otherwise a ConfigMap-only apply silently
        # has no effect until a manual restart.
        annotations = {
          "checksum/policy"          = sha256(local.policy_json)
          "checksum/governed-policy" = sha256(local.governed_policy_json)
        }
      }

      spec {
        service_account_name = kubernetes_service_account.broker.metadata[0].name
        # The broker authenticates its own TokenReview calls with this token.
        automount_service_account_token = true
        node_selector                   = var.node_selector

        # Private GHCR image.
        image_pull_secrets {
          name = var.ghcr_pull_secret_name
        }

        security_context {
          run_as_non_root = true
          run_as_user     = 65532
          run_as_group    = 65532
          fs_group        = 65532
          seccomp_profile {
            type = "RuntimeDefault"
          }
        }

        container {
          name              = local.name
          image             = var.image
          image_pull_policy = "IfNotPresent"

          port {
            name           = "http"
            container_port = 8080
          }

          port {
            name           = "metrics"
            container_port = 9090
          }

          port {
            name           = "lake-s3"
            container_port = 8081
          }

          env {
            name  = "S3_ENDPOINT"
            value = var.s3_endpoint
          }
          env {
            name  = "S3_REGION"
            value = var.s3_region
          }
          env {
            name  = "OIDC_ISSUER"
            value = var.oidc_issuer
          }
          env {
            name  = "OIDC_AUDIENCES"
            value = join(",", var.oidc_audiences)
          }
          env {
            name  = "TOKEN_AUDIENCE"
            value = var.token_audience
          }
          env {
            name  = "POLICY_PATH"
            value = "/etc/storage-broker/policy.json"
          }
          env {
            name  = "GOVERNED_POLICY_PATH"
            value = "/etc/storage-broker/governed-policy.json"
          }
          env {
            name  = "GOVERNED_SCHEMA"
            value = "storage_broker"
          }
          env {
            name  = "DUCKLAKE_ROOT"
            value = var.ducklake_root
          }
          env {
            name  = "DUCKLAKE_MAX_PUT_BYTES"
            value = tostring(var.ducklake_max_put_bytes)
          }
          env {
            name  = "LAKE_OPERATION_TIMEOUT"
            value = var.lake_operation_timeout
          }
          env {
            name  = "LAKE_IO_IDLE_TIMEOUT"
            value = var.lake_io_idle_timeout
          }
          env {
            name  = "LAKE_MAX_CONCURRENCY"
            value = tostring(var.lake_max_concurrency)
          }
          env {
            name = "DATABASE_URL"
            value_from {
              secret_key_ref {
                name = kubernetes_secret.broker_database.metadata[0].name
                key  = "DATABASE_URL"
              }
            }
          }

          env {
            name = "S3_ACCESS_KEY_ID"
            value_from {
              secret_key_ref {
                name = kubernetes_secret.broker_s3.metadata[0].name
                key  = "S3_ACCESS_KEY_ID"
              }
            }
          }
          env {
            name = "S3_SECRET_ACCESS_KEY"
            value_from {
              secret_key_ref {
                name = kubernetes_secret.broker_s3.metadata[0].name
                key  = "S3_SECRET_ACCESS_KEY"
              }
            }
          }

          resources {
            requests = var.resources.requests
            limits   = var.resources.limits
          }

          security_context {
            allow_privilege_escalation = false
            read_only_root_filesystem  = true
            run_as_non_root            = true
            run_as_user                = 65532
            capabilities {
              drop = ["ALL"]
            }
          }

          liveness_probe {
            http_get {
              path = "/healthz"
              port = 8080
            }
            initial_delay_seconds = 5
            period_seconds        = 20
            failure_threshold     = 3
          }

          readiness_probe {
            http_get {
              path = "/readyz"
              port = 8080
            }
            initial_delay_seconds = 3
            period_seconds        = 10
          }

          volume_mount {
            name       = "policy"
            mount_path = "/etc/storage-broker"
            read_only  = true
          }
          volume_mount {
            name       = "postgresql-ca"
            mount_path = "/etc/ssl/pg"
            read_only  = true
          }
        }

        volume {
          name = "policy"
          config_map {
            name = kubernetes_config_map.broker_policy.metadata[0].name
          }
        }
        volume {
          name = "postgresql-ca"
          secret {
            secret_name = kubernetes_secret.broker_pg_ca.metadata[0].name
          }
        }

        # Spread replicas across nodes for HA.
        affinity {
          pod_anti_affinity {
            required_during_scheduling_ignored_during_execution {
              topology_key = "kubernetes.io/hostname"
              label_selector {
                match_labels = local.selector_labels
              }
            }
          }
        }
      }
    }
  }
}

resource "kubernetes_service_v1" "broker" {
  metadata {
    name      = local.name
    namespace = var.namespace
    labels    = local.labels
  }

  spec {
    selector = local.selector_labels

    port {
      name        = "http"
      port        = 80
      target_port = 8080
      protocol    = "TCP"
    }

    port {
      name        = "lake-s3"
      port        = 81
      target_port = 8081
      protocol    = "TCP"
    }

    type = "ClusterIP"
  }
}

resource "kubernetes_pod_disruption_budget_v1" "broker" {
  metadata {
    name      = local.name
    namespace = var.namespace
    labels    = local.labels
  }

  spec {
    min_available = 1
    selector {
      match_labels = local.selector_labels
    }
  }
}

# --- Network policies ------------------------------------------------------
# Egress: outbound 443 to the public internet (the Infomaniak S3 gateway and
# Zitadel via the Cloudflare edge). DNS (kube-system:53) and the K8s API
# (TokenReview, post-DNAT endpoint) are already allowed for all infra pods by the
# infra-wide allow-dns / allow-apiserver-egress policies.
resource "kubernetes_network_policy" "broker_egress" {
  metadata {
    name      = "allow-storage-broker-egress"
    namespace = var.namespace
    labels    = local.labels
  }

  spec {
    pod_selector {
      match_labels = local.selector_labels
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

# Ingress: tenant applications reach only the broker API. Observability
# collectors reach only the separate metrics listener.
resource "kubernetes_network_policy" "broker_ingress" {
  metadata {
    name      = "allow-storage-broker-ingress"
    namespace = var.namespace
    labels    = local.labels
  }

  spec {
    pod_selector {
      match_labels = local.selector_labels
    }

    policy_types = ["Ingress"]

    ingress {
      from {
        namespace_selector {
          match_labels = {
            "platform.infra/tenant" = "true"
          }
        }
      }
      ports {
        protocol = "TCP"
        port     = "8080"
      }
    }

    ingress {
      from {
        namespace_selector {
          match_labels = {
            "platform.infra/tenant" = "true"
          }
        }
      }
      ports {
        protocol = "TCP"
        port     = "8081"
      }
    }

    ingress {
      from {
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
        port     = "9090"
      }
    }
  }
}
