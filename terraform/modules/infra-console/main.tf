# =============================================================================
# infra-console — read-only console and MCP surface over platform state.
#
# See docs/plans/infra-console.md. It keeps the storage-broker's distroless
# non-root image, digest pin, and explicit egress posture, but defaults to one
# replica because this read-only operator surface tolerates brief downtime.
#
# The console is stateless and writes nothing anywhere. It holds no database of
# its own; every panel is read from the system that already owns that state.
# =============================================================================

locals {
  name = "infra-console"

  labels = {
    "app.kubernetes.io/name"       = local.name
    "app.kubernetes.io/component"  = "console"
    "app.kubernetes.io/part-of"    = "platform-infra"
    "app.kubernetes.io/managed-by" = "opentofu"
  }

  selector_labels = {
    "app.kubernetes.io/name" = local.name
  }
}

resource "kubernetes_service_account" "console" {
  metadata {
    name      = local.name
    namespace = var.namespace
    labels    = local.labels
  }
}

# -----------------------------------------------------------------------------
# RBAC — read-only, and deliberately WITHOUT secrets under any verb.
#
# That exclusion is load-bearing (docs/plans/infra-console.md, AC2). It is why
# shared-service chart versions are read from the `helm.sh/chart` LABEL on
# Deployments rather than from Helm's release Secrets.
#
# configmaps and pods/log are NOT cluster-wide. Helm release ConfigMaps carry
# the whole rendered release, which can include templated Secret objects, and
# pod logs routinely contain tokens — so both are granted per tenant namespace
# below, plus one resourceName-scoped read of the storage broker's policy.
# Every call site is already namespace-gated; this makes the grant agree.
# -----------------------------------------------------------------------------
resource "kubernetes_cluster_role" "console" {
  metadata {
    name   = local.name
    labels = local.labels
  }

  rule {
    api_groups = [""]
    resources = [
      "nodes",
      "namespaces",
      "pods",
      "services",
      "resourcequotas",
      "limitranges",
      "events",
    ]
    verbs = ["get", "list", "watch"]
  }

  rule {
    api_groups = ["apps"]
    resources  = ["deployments", "replicasets", "statefulsets"]
    verbs      = ["get", "list", "watch"]
  }

  rule {
    api_groups = ["batch"]
    resources  = ["jobs", "cronjobs"]
    verbs      = ["get", "list", "watch"]
  }

  rule {
    api_groups = ["networking.k8s.io"]
    resources  = ["ingresses", "networkpolicies"]
    verbs      = ["get", "list", "watch"]
  }

  rule {
    api_groups = ["policy"]
    resources  = ["poddisruptionbudgets"]
    verbs      = ["get", "list", "watch"]
  }

  rule {
    api_groups = ["cert-manager.io"]
    resources  = ["certificates"]
    verbs      = ["get", "list", "watch"]
  }

  rule {
    api_groups = ["metrics.k8s.io"]
    resources  = ["nodes", "pods"]
    verbs      = ["get", "list"]
  }

  # NOTE: "secrets" is absent on purpose. Do not add it. If a future panel
  # appears to need it, read the same information from a label or an owning
  # object instead — that is how chart versions are already handled.
}

resource "kubernetes_cluster_role_binding" "console" {
  metadata {
    name   = local.name
    labels = local.labels
  }

  role_ref {
    api_group = "rbac.authorization.k8s.io"
    kind      = "ClusterRole"
    name      = kubernetes_cluster_role.console.metadata[0].name
  }

  subject {
    kind      = "ServiceAccount"
    name      = kubernetes_service_account.console.metadata[0].name
    namespace = var.namespace
  }
}

# -----------------------------------------------------------------------------
# Upstream credentials. Each is optional; an empty value leaves that source
# reporting itself unavailable.
# -----------------------------------------------------------------------------
# Helm release history and pod logs, per tenant namespace.
#
# Deployments (release ConfigMaps) and the workload log tail are the only two
# reads that need these, and both already refuse a namespace without the
# platform.infra/tenant label. Granting them here keeps the ClusterRole free of
# both, so a leaked ServiceAccount token cannot read infra's own pod logs or
# every tenant's rendered release.
resource "kubernetes_role" "tenant_reader" {
  for_each = var.tenant_namespaces

  metadata {
    name      = local.name
    namespace = each.value
    labels    = local.labels
  }

  rule {
    api_groups = [""]
    resources  = ["configmaps"]
    verbs      = ["get", "list"]
  }

  rule {
    api_groups = [""]
    resources  = ["pods/log"]
    verbs      = ["get"]
  }
}

resource "kubernetes_role_binding" "tenant_reader" {
  for_each = var.tenant_namespaces

  metadata {
    name      = local.name
    namespace = each.value
    labels    = local.labels
  }

  role_ref {
    api_group = "rbac.authorization.k8s.io"
    kind      = "Role"
    name      = kubernetes_role.tenant_reader[each.key].metadata[0].name
  }

  subject {
    kind      = "ServiceAccount"
    name      = kubernetes_service_account.console.metadata[0].name
    namespace = var.namespace
  }
}

# The storage broker's policy, and nothing else in this namespace.
# resourceNames works with get, which is the only verb BucketFor uses.
resource "kubernetes_role" "broker_policy_reader" {
  metadata {
    name      = "${local.name}-broker-policy"
    namespace = var.namespace
    labels    = local.labels
  }

  rule {
    api_groups     = [""]
    resources      = ["configmaps"]
    resource_names = ["storage-broker-policy"]
    verbs          = ["get"]
  }
}

resource "kubernetes_role_binding" "broker_policy_reader" {
  metadata {
    name      = "${local.name}-broker-policy"
    namespace = var.namespace
    labels    = local.labels
  }

  role_ref {
    api_group = "rbac.authorization.k8s.io"
    kind      = "Role"
    name      = kubernetes_role.broker_policy_reader.metadata[0].name
  }

  subject {
    kind      = "ServiceAccount"
    name      = kubernetes_service_account.console.metadata[0].name
    namespace = var.namespace
  }
}

resource "kubernetes_secret" "console" {
  metadata {
    name      = "${local.name}-credentials"
    namespace = var.namespace
    labels    = local.labels
  }

  data = {
    GITHUB_TOKEN                        = var.github_token
    CLOUDFLARE_API_TOKEN                = var.cloudflare_api_token
    ZITADEL_TOKEN                       = var.zitadel_token
    S3_ACCESS_KEY_ID                    = var.s3_access_key_id
    S3_SECRET_ACCESS_KEY                = var.s3_secret_access_key
    SWIFT_APPLICATION_CREDENTIAL_ID     = var.swift_application_credential_id
    SWIFT_APPLICATION_CREDENTIAL_SECRET = var.swift_application_credential_secret
    POSTGRES_PASSWORD                   = var.postgres_password
  }

  type = "Opaque"
}

# The PostgreSQL CA, so the console can verify the server certificate rather
# than merely encrypting to it.
resource "kubernetes_secret" "pg_ca" {
  count = var.pg_ca_enabled ? 1 : 0

  metadata {
    name      = "${local.name}-pg-ca"
    namespace = var.namespace
    labels    = local.labels
  }

  data = {
    "ca.crt" = var.pg_ca_cert
  }
}

resource "kubernetes_deployment_v1" "console" {
  metadata {
    name      = local.name
    namespace = var.namespace
    labels    = local.labels
  }

  # Egress and RBAC must exist before pods start, or the first API call and
  # upstream fetch are rejected.
  depends_on = [
    kubernetes_network_policy.console_egress,
    kubernetes_cluster_role_binding.console,
  ]

  spec {
    replicas = var.replicas

    selector {
      match_labels = local.selector_labels
    }

    template {
      metadata {
        labels = local.labels
        # Roll the Deployment when a credential changes; the console reads its
        # configuration once at startup.
        annotations = {
          "checksum/credentials" = sha256(join(",", [
            var.github_token,
            var.cloudflare_api_token,
            var.zitadel_token,
            var.s3_access_key_id,
            var.s3_secret_access_key,
            var.swift_application_credential_id,
            var.swift_application_credential_secret,
            var.postgres_password,
          ]))
        }
      }

      spec {
        # Spread replicas across nodes when redundancy is enabled. Without this
        # both can land on one node and the optional PDB protects nothing.
        affinity {
          pod_anti_affinity {
            preferred_during_scheduling_ignored_during_execution {
              weight = 100
              pod_affinity_term {
                topology_key = "kubernetes.io/hostname"
                label_selector {
                  match_labels = local.selector_labels
                }
              }
            }
          }
        }

        service_account_name            = kubernetes_service_account.console.metadata[0].name
        automount_service_account_token = true
        node_selector                   = var.node_selector

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

        dynamic "volume" {
          for_each = var.pg_ca_enabled ? [1] : []
          content {
            name = "pg-ca"
            secret {
              secret_name = kubernetes_secret.pg_ca[0].metadata[0].name
            }
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

          env {
            name  = "LOG_LEVEL"
            value = var.log_level
          }
          env {
            name  = "CLOUDFLARE_ACCOUNT_ID"
            value = var.cloudflare_account_id
          }
          env {
            name  = "ZITADEL_URL"
            value = var.zitadel_url
          }
          # Enables authentication. The service logs a loud warning and serves
          # unauthenticated when this is empty, which is local-development mode
          # only — a deployment must always set it.
          env {
            name  = "OIDC_ISSUER"
            value = var.oidc_issuer
          }
          # Browser sign-in. A PKCE public client, so no secret is involved.
          # Empty means bearer-token callers still work and the UI cannot sign
          # in — the service logs that distinction at startup.
          env {
            name  = "OIDC_CLIENT_ID"
            value = var.oidc_client_id
          }
          env {
            name  = "CONSOLE_URL"
            value = "https://${var.console_domain}"
          }
          env {
            name  = "POSTGRES_HOST"
            value = var.postgres_host
          }
          env {
            name  = "POSTGRES_DATABASE"
            value = var.postgres_database
          }
          env {
            name  = "POSTGRES_USER"
            value = var.postgres_user
          }
          env {
            name  = "POSTGRES_SSLROOTCERT"
            value = "/etc/ssl/pg/ca.crt"
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
            name  = "SWIFT_AUTH_URL"
            value = var.swift_auth_url
          }
          env {
            name  = "SWIFT_ENDPOINT"
            value = var.swift_endpoint
          }

          env_from {
            secret_ref {
              name = kubernetes_secret.console.metadata[0].name
            }
          }

          liveness_probe {
            http_get {
              path = "/healthz"
              port = "http"
            }
            initial_delay_seconds = 5
            period_seconds        = 15
          }

          # Readiness deliberately does NOT depend on upstream health: a
          # degraded source is reported per Section, and pulling the console
          # out of service when one upstream is slow removes the tool you
          # opened to diagnose it.
          readiness_probe {
            http_get {
              path = "/readyz"
              port = "http"
            }
            initial_delay_seconds = 3
            period_seconds        = 10
          }

          resources {
            requests = {
              cpu    = var.resources.requests.cpu
              memory = var.resources.requests.memory
            }
            limits = {
              cpu    = var.resources.limits.cpu
              memory = var.resources.limits.memory
            }
          }

          # verify-ca needs the CA on disk. Mounted at the same path the
          # tenant DATABASE_URL uses, so both speak the identical convention.
          dynamic "volume_mount" {
            for_each = var.pg_ca_enabled ? [1] : []
            content {
              name       = "pg-ca"
              mount_path = "/etc/ssl/pg"
              read_only  = true
            }
          }

          security_context {
            allow_privilege_escalation = false
            read_only_root_filesystem  = true
            capabilities {
              drop = ["ALL"]
            }
          }
        }
      }
    }
  }
}

resource "kubernetes_service_v1" "console" {
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

    type = "ClusterIP"
  }
}

resource "kubernetes_pod_disruption_budget_v1" "console" {
  # @lore A minAvailable PDB on one replica blocks node drains indefinitely.
  count = var.replicas > 1 ? 1 : 0

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

# --- Network policies --------------------------------------------------------
# Egress: the public internet on 443 (Zitadel via the edge, Cloudflare,
# GitHub, object storage). DNS, the API server and PostgreSQL come from the
# infra-wide policies that select every pod in the namespace. plus 443 to the
# internet for Zitadel, S3, Cloudflare and GitHub. DNS and the K8s API are
# already allowed for every infra pod by the infra-wide policies.
resource "kubernetes_network_policy" "console_egress" {
  metadata {
    name      = "allow-infra-console-egress"
    namespace = var.namespace
    labels    = local.labels
  }

  spec {
    pod_selector {
      match_labels = local.selector_labels
    }

    policy_types = ["Egress"]

    # Public internet on 443, with RFC1918 excluded so this rule cannot be
    # used to reach in-cluster services implicitly.
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

    # PostgreSQL egress is already granted to every pod in this namespace by
    # kubernetes_network_policy.infra_allow_postgres_egress; a second rule here
    # only disguised the fact that the matching pg-ingress rule was missing.
  }
}

# Ingress: only from ingress-nginx. The console is reached through the tunnel,
# never directly from a tenant namespace.
resource "kubernetes_network_policy" "console_ingress" {
  metadata {
    name      = "allow-infra-console-ingress"
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
        pod_selector {
          match_labels = {
            "app.kubernetes.io/name" = "ingress-nginx"
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

# -----------------------------------------------------------------------------
# Ingress. No TLS block: TLS terminates at the Cloudflare edge (Universal SSL)
# and the request reaches nginx over the tunnel as plain HTTP — the same shape
# as kubernetes_ingress_v1.zitadel. The console sets its session cookie's
# Secure flag set unconditionally rather than derived from the origin connection.
# -----------------------------------------------------------------------------
resource "kubernetes_ingress_v1" "console" {
  metadata {
    name      = local.name
    namespace = var.namespace
    labels    = local.labels

    annotations = {
      "nginx.ingress.kubernetes.io/limit-rps"              = "20"
      "nginx.ingress.kubernetes.io/limit-burst-multiplier" = "5"
      "nginx.ingress.kubernetes.io/limit-connections"      = "10"
    }
  }

  spec {
    ingress_class_name = "nginx"

    rule {
      host = var.console_domain

      http {
        path {
          path      = "/"
          path_type = "Prefix"

          backend {
            service {
              name = kubernetes_service_v1.console.metadata[0].name
              port {
                number = 80
              }
            }
          }
        }
      }
    }
  }

  depends_on = [kubernetes_service_v1.console]
}
