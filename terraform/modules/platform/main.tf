# =============================================================================
# Namespaces
# =============================================================================
resource "kubernetes_namespace" "infra" {
  metadata {
    name = "infra"
    labels = {
      "kubernetes.io/metadata.name" = "infra"
      "platform.infra/role"         = "shared-services"

      # PSA: baseline enforce, restricted warn/audit. Cannot upgrade to restricted enforce
      # because ingress-nginx requires NET_BIND_SERVICE capability. Restricted warn catches
      # non-compliant pods. If ingress-nginx moves to its own namespace, upgrade infra to restricted.
      "pod-security.kubernetes.io/enforce" = "baseline"
      "pod-security.kubernetes.io/warn"    = "restricted"
      "pod-security.kubernetes.io/audit"   = "restricted"
    }
  }


}

resource "kubernetes_default_service_account" "infra" {
  metadata {
    namespace = kubernetes_namespace.infra.metadata[0].name
  }

  automount_service_account_token = false
}

# The node Collector must read /var/log/pods through a read-only hostPath.
# Keep that Pod Security exception out of the shared infra namespace.
resource "kubernetes_namespace" "observability" {
  count = var.observability_enabled ? 1 : 0

  metadata {
    name = "observability"
    labels = {
      "kubernetes.io/metadata.name"        = "observability"
      "platform.infra/role"                = "node-observability"
      "pod-security.kubernetes.io/enforce" = "privileged"
      "pod-security.kubernetes.io/warn"    = "baseline"
      "pod-security.kubernetes.io/audit"   = "baseline"
    }
  }


}

resource "kubernetes_default_service_account" "observability" {
  count = var.observability_enabled ? 1 : 0

  metadata {
    namespace = kubernetes_namespace.observability[0].metadata[0].name
  }

  automount_service_account_token = false
}

# =============================================================================
# Infra namespace network policies
# =============================================================================
resource "kubernetes_network_policy" "infra_default_deny" {
  metadata {
    name      = "default-deny"
    namespace = kubernetes_namespace.infra.metadata[0].name
  }

  spec {
    pod_selector {}

    policy_types = ["Ingress", "Egress"]
  }
}

resource "kubernetes_network_policy" "infra_allow_dns" {
  metadata {
    name      = "allow-dns"
    namespace = kubernetes_namespace.infra.metadata[0].name
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

# Allow all infra pods to reach the Kubernetes API server.
#
# NetworkPolicy egress is enforced POST-DNAT here: pods dial the in-cluster
# Service (10.96.0.1:443) but kube-proxy rewrites it to the Infomaniak managed
# control-plane endpoint, so the rule must target that endpoint (IP:nodePort),
# NOT :443. Without it, every fresh pod that talks to the API (cert-manager +
# cainjector, Helm hook jobs, ingress-nginx, ...) hangs
# at API-client init and crash-loops; existing pods only survive on
# long-established connections. Endpoint from:
#   kubectl get endpoints kubernetes -n default
resource "kubernetes_network_policy" "infra_allow_apiserver_egress" {
  metadata {
    name      = "allow-apiserver-egress"
    namespace = kubernetes_namespace.infra.metadata[0].name
  }

  spec {
    pod_selector {}

    policy_types = ["Egress"]

    egress {
      to {
        ip_block {
          cidr = var.apiserver_cidr
        }
      }
      ports {
        protocol = "TCP"
        port     = var.apiserver_port
      }
    }
  }
}

resource "kubernetes_network_policy" "infra_allow_cert_manager_webhook" {
  metadata {
    name      = "allow-cert-manager-webhook"
    namespace = kubernetes_namespace.infra.metadata[0].name
  }

  spec {
    pod_selector {
      match_labels = {
        "app.kubernetes.io/name"     = "webhook"
        "app.kubernetes.io/instance" = "cert-manager"
      }
    }

    policy_types = ["Ingress"]

    ingress {
      ports {
        protocol = "TCP"
        port     = "10250"
      }
    }
  }
}

resource "kubernetes_network_policy" "infra_allow_ingress_nginx_webhook" {
  metadata {
    name      = "allow-ingress-nginx-webhook"
    namespace = kubernetes_namespace.infra.metadata[0].name
  }

  spec {
    pod_selector {
      match_labels = {
        "app.kubernetes.io/name"      = "ingress-nginx"
        "app.kubernetes.io/component" = "controller"
      }
    }

    policy_types = ["Ingress"]

    ingress {
      ports {
        protocol = "TCP"
        port     = "8443"
      }
    }
  }
}

# Allow Helm hook jobs (admission-create, zitadel-init/setup) to reach the K8s API server.
# These jobs need egress to the API server to create/update secrets and run migrations.
resource "kubernetes_network_policy" "infra_allow_hook_jobs_egress" {
  metadata {
    name      = "allow-hook-jobs-egress"
    namespace = kubernetes_namespace.infra.metadata[0].name
  }

  spec {
    pod_selector {
      match_expressions {
        key      = "job-name"
        operator = "Exists"
      }
    }

    policy_types = ["Egress"]

    egress {
      ports {
        protocol = "TCP"
        port     = "443"
      }
      ports {
        protocol = "TCP"
        port     = "5432"
      }
    }
  }
}

# Node-local observability collectors scrape the controller's metrics endpoint.
# The response includes every tenant's Ingress names and request counts, so no
# other source may reach this port.
resource "kubernetes_network_policy" "infra_allow_collector_to_ingress_nginx_metrics" {
  metadata {
    name      = "allow-collector-to-ingress-nginx-metrics"
    namespace = kubernetes_namespace.infra.metadata[0].name
  }

  spec {
    pod_selector {
      match_labels = {
        "app.kubernetes.io/name"      = "ingress-nginx"
        "app.kubernetes.io/component" = "controller"
      }
    }

    policy_types = ["Ingress"]

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
        port     = "10254"
      }
    }
  }
}

# Allow ingress-nginx controller pods to receive external traffic and route to tenant namespaces
resource "kubernetes_network_policy" "infra_allow_ingress_external" {
  metadata {
    name      = "allow-ingress-external"
    namespace = kubernetes_namespace.infra.metadata[0].name
  }

  spec {
    pod_selector {
      match_labels = {
        "app.kubernetes.io/name" = "ingress-nginx"
      }
    }

    policy_types = ["Ingress", "Egress"]

    # Request traffic from any source on the HTTP/HTTPS ports only. The
    # controller's :10254 endpoint (metrics, health) is reachable solely by the
    # observability collectors (infra_allow_collector_to_ingress_nginx_metrics);
    # the admission webhook keeps its own 8443 rule.
    ingress {
      ports {
        protocol = "TCP"
        port     = "80"
      }

      ports {
        protocol = "TCP"
        port     = "443"
      }
    }

    egress {
      to {
        # Route to tenant namespaces only (not infra-internal services like PG)
        namespace_selector {
          match_labels = {
            "platform.infra/tenant" = "true"
          }
        }
      }

      ports {
        protocol = "TCP"
        port     = "80"
      }

      ports {
        protocol = "TCP"
        port     = "443"
      }

      ports {
        protocol = "TCP"
        port     = "3001"
      }

      ports {
        protocol = "TCP"
        port     = "5173"
      }

      ports {
        protocol = "TCP"
        port     = "8080"
      }
    }

    # Allow egress from ingress-nginx to Zitadel pods within the infra namespace
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

    # Allow egress from ingress-nginx to the infra console. Without this the
    # console's Ingress admits traffic that nginx is never permitted to send,
    # and the edge returns 502. Egress here is enumerated per workload, so a
    # new infra-namespace service behind nginx needs its own rule.
    egress {
      to {
        namespace_selector {
          match_labels = {
            "kubernetes.io/metadata.name" = "infra"
          }
        }

        pod_selector {
          match_labels = {
            "app.kubernetes.io/name" = "infra-console"
          }
        }
      }

      ports {
        protocol = "TCP"
        port     = "8080"
      }
    }

    # Allow egress from ingress-nginx to the Zitadel v2 login UI (port 3000).
    egress {
      to {
        namespace_selector {
          match_labels = {
            "kubernetes.io/metadata.name" = "infra"
          }
        }

        pod_selector {
          match_labels = {
            "app.kubernetes.io/name" = "zitadel-login"
          }
        }
      }

      ports {
        protocol = "TCP"
        port     = "3000"
      }
    }

    # Allow egress to the default backend ("nothing deployed" page) in infra.
    egress {
      to {
        namespace_selector {
          match_labels = {
            "kubernetes.io/metadata.name" = "infra"
          }
        }

        pod_selector {
          match_labels = {
            "app.kubernetes.io/name" = "nothing-deployed"
          }
        }
      }

      ports {
        protocol = "TCP"
        port     = "8080"
      }
    }
    # NOTE: egress to the Kubernetes API server is granted to all infra pods by
    # kubernetes_network_policy.infra_allow_apiserver_egress.
  }
}

# Allow cert-manager internet egress (ACME HTTP-01/DNS-01 challenges)
resource "kubernetes_network_policy" "infra_certmanager_internet_egress" {
  metadata {
    name      = "allow-certmanager-internet-egress"
    namespace = kubernetes_namespace.infra.metadata[0].name
  }

  spec {
    pod_selector {
      match_labels = {
        "app.kubernetes.io/name" = "cert-manager"
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

# Allow Zitadel internet egress (OIDC federation, SMTP)
resource "kubernetes_network_policy" "infra_zitadel_internet_egress" {
  metadata {
    name      = "allow-zitadel-internet-egress"
    namespace = kubernetes_namespace.infra.metadata[0].name
  }

  spec {
    pod_selector {
      match_labels = {
        "app.kubernetes.io/name" = "zitadel"
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

      ports {
        protocol = "TCP"
        port     = "587"
      }
    }
  }
}

# Allow PostgreSQL egress within the cluster (infra pods -> postgresql pod)
resource "kubernetes_network_policy" "infra_allow_postgres_egress" {
  metadata {
    name      = "allow-postgres-egress"
    namespace = kubernetes_namespace.infra.metadata[0].name
  }

  spec {
    pod_selector {}

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

resource "kubernetes_network_policy" "infra_pg_ingress" {
  metadata {
    name      = "pg-ingress"
    namespace = kubernetes_namespace.infra.metadata[0].name
  }

  spec {
    pod_selector {
      match_labels = {
        "app.kubernetes.io/name" = "postgresql"
      }
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
        port     = "5432"
      }
    }

    ingress {
      from {
        pod_selector {
          match_labels = {
            "app.kubernetes.io/name" = "zitadel"
          }
        }
      }

      ports {
        protocol = "TCP"
        port     = "5432"
      }
    }

    ingress {
      from {
        pod_selector {
          match_labels = {
            "app" = "pg-backup"
          }
        }
      }

      ports {
        protocol = "TCP"
        port     = "5432"
      }
    }

    # The infra console reads as platform_reader. Same namespace, so a pod
    # selector is enough; without it the database panel hangs rather than
    # reporting itself unavailable, because a dropped packet is not a refusal.
    ingress {
      from {
        pod_selector {
          match_labels = {
            "app.kubernetes.io/name" = "infra-console"
          }
        }
      }

      ports {
        protocol = "TCP"
        port     = "5432"
      }
    }

    # Governed object authorization rows live in their own broker-owned schema.
    # The broker role cannot see tenant schemas; this edge admits only the
    # exact storage-broker workload on PostgreSQL's application port.
    ingress {
      from {
        pod_selector {
          match_labels = {
            "app.kubernetes.io/name" = "storage-broker"
          }
        }
      }

      ports {
        protocol = "TCP"
        port     = "5432"
      }
    }

    ingress {
      from {
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

    # Collectors read only the existing PostgreSQL exporter endpoint.
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
        port     = "9187"
      }
    }
  }
}

# =============================================================================
# PostgreSQL (self-hosted via Bitnami Helm chart)
# =============================================================================
resource "helm_release" "postgresql" {
  name       = "postgresql"
  repository = "oci://registry-1.docker.io/bitnamicharts"
  chart      = "postgresql"
  version    = "18.7.0"
  namespace  = kubernetes_namespace.infra.metadata[0].name
  timeout    = 600

  values = [lookup(var.helm_values, "postgresql", file("${path.module}/../../../helm/values/postgresql.yaml"))]

  # @lore These must stay set_sensitive: plain set blocks expose both live
  # PostgreSQL control-plane credentials in OpenTofu plan output.
  set_sensitive {
    name  = "auth.postgresPassword"
    value = var.pg_admin_password
  }

  set_sensitive {
    name  = "auth.replicationPassword"
    value = var.pg_replication_password
  }

  # The deployment orders this module after its worker pool.
  depends_on = [
  ]
}

# Native, inert-until-called PostgreSQL tooling for evidence-driven index
# stewardship. These extensions do not refresh or rebuild indexes. amcheck
# verifies structural invariants; pgstattuple measures a specifically named
# relation when an operator investigates suspected bloat.
resource "postgresql_extension" "amcheck_postgres" {
  name     = "amcheck"
  database = "postgres"

  depends_on = [helm_release.postgresql]
}

resource "postgresql_extension" "pgstattuple_postgres" {
  name     = "pgstattuple"
  database = "postgres"

  depends_on = [helm_release.postgresql]
}

# @lore StatefulSet volumeClaimTemplates are immutable. Helm keeps the 10Gi
# bootstrap template, while OpenTofu owns and safely expands the bound claims.
resource "kubernetes_persistent_volume_claim_v1" "postgresql_data" {
  for_each = {
    "data-postgresql-primary-0" = "primary"
    "data-postgresql-read-0"    = "read"
  }

  metadata {
    name      = each.key
    namespace = kubernetes_namespace.infra.metadata[0].name
    labels = {
      "app.kubernetes.io/component" = each.value
      "app.kubernetes.io/instance"  = "postgresql"
      "app.kubernetes.io/name"      = "postgresql"
    }
  }

  spec {
    access_modes       = ["ReadWriteOnce"]
    storage_class_name = var.postgresql_storage_class
    volume_mode        = "Filesystem"

    resources {
      requests = {
        storage = var.postgresql_storage_size
      }
    }
  }

  lifecycle {
    prevent_destroy = true
  }
}



# Metrics API used by kubectl top and Kubernetes autoscaling. PostgreSQL exposes
# database-specific metrics through its exporter sidecar; a Prometheus deployment
# can scrape the annotated service without changing the database release.
resource "helm_release" "metrics_server" {
  name       = "metrics-server"
  repository = "https://kubernetes-sigs.github.io/metrics-server/"
  chart      = "metrics-server"
  version    = "3.13.0"
  namespace  = "kube-system"
  timeout    = 300

  values = [lookup(var.helm_values, "metrics-server", file("${path.module}/../../../helm/values/metrics-server.yaml"))]

  depends_on = [
  ]
}

# Read the auto-generated PostgreSQL CA certificate (created by Bitnami chart
# with tls.autoGenerated: true). Distributed to tenant namespaces and Zitadel
# for sslmode=verify-ca connections.
data "kubernetes_secret" "postgresql_ca" {
  metadata {
    name      = "postgresql-crt"
    namespace = kubernetes_namespace.infra.metadata[0].name
  }

  depends_on = [helm_release.postgresql]
}

# =============================================================================
# PostgreSQL backup CronJob — daily per-database pg_dump to Infomaniak S3
# =============================================================================

# Allow backup pods (label: app=pg-backup) to reach PostgreSQL
resource "kubernetes_network_policy" "infra_allow_pg_backup_egress" {
  metadata {
    name      = "allow-pg-backup-egress"
    namespace = kubernetes_namespace.infra.metadata[0].name
  }

  spec {
    pod_selector {
      match_labels = {
        "app" = "pg-backup"
      }
    }

    policy_types = ["Egress"]

    # Reach PostgreSQL primary in the infra namespace
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

    # Reach Infomaniak S3 (internet egress — S3 is external)
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

    # DNS resolution
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

  depends_on = [kubernetes_namespace.infra]
}

resource "random_password" "pg_backup_password" {
  length  = 32
  special = false
}

resource "postgresql_role" "pg_backup" {
  name     = "infra_backup"
  login    = true
  password = random_password.pg_backup_password.result

  lifecycle {
    ignore_changes = [password]
  }

  depends_on = [helm_release.postgresql]
}

resource "postgresql_grant" "pg_backup_connect_postgres" {
  database    = "postgres"
  role        = postgresql_role.pg_backup.name
  object_type = "database"
  privileges  = ["CONNECT"]

  depends_on = [postgresql_role.pg_backup]
}

# =============================================================================
# platform_reader — the infra console's read-only database identity
#
# The console is outside the schema-per-app boundary by design: each tenant role
# owns its own schema and nothing else can see in. This role is the declared,
# per-tenant exception that lets the console render a database panel.
#
# It holds SELECT and nothing else. The grants are per schema, visible in git,
# and revocable one tenant at a time — so isolation stays structural rather
# than becoming an assumption. See docs/plans/infra-console.md.
# =============================================================================
resource "random_password" "platform_reader_password" {
  length  = 32
  special = false
}

resource "postgresql_role" "platform_reader" {
  name     = "platform_reader"
  login    = true
  password = random_password.platform_reader_password.result

  # A console query must never be able to hold the shared instance. The row
  # browser also bounds itself, but the ceiling belongs on the role: it applies
  # to every statement regardless of which code path issued it.
  statement_timeout = 5000

  lifecycle {
    ignore_changes = [password]
  }

  depends_on = [helm_release.postgresql]
}

resource "postgresql_grant" "platform_reader_connect" {
  database    = "postgres"
  role        = postgresql_role.platform_reader.name
  object_type = "database"
  privileges  = ["CONNECT"]

  depends_on = [postgresql_role.platform_reader]
}

locals {
  zitadel_schemas = ["eventstore", "projections", "system", "auth", "adminapi", "queue", "cache", "logstore"]
}

resource "random_password" "zitadel_backup_password" {
  length  = 32
  special = false
}

resource "postgresql_role" "zitadel_backup" {
  name     = "zitadel_backup"
  login    = true
  password = random_password.zitadel_backup_password.result

  lifecycle {
    ignore_changes = [password]
  }

  depends_on = [helm_release.postgresql]
}

resource "postgresql_grant" "zitadel_backup_connect" {
  database    = postgresql_database.zitadel.name
  role        = postgresql_role.zitadel_backup.name
  object_type = "database"
  privileges  = ["CONNECT"]

  depends_on = [postgresql_role.zitadel_backup, postgresql_database.zitadel]
}

resource "postgresql_grant" "zitadel_backup_usage" {
  for_each = toset(local.zitadel_schemas)

  database    = postgresql_database.zitadel.name
  role        = postgresql_role.zitadel_backup.name
  schema      = each.value
  object_type = "schema"
  privileges  = ["USAGE"]

  depends_on = [postgresql_grant.zitadel_backup_connect]
}

resource "postgresql_grant" "zitadel_backup_select" {
  for_each = toset(local.zitadel_schemas)

  database    = postgresql_database.zitadel.name
  role        = postgresql_role.zitadel_backup.name
  schema      = each.value
  object_type = "table"
  privileges  = ["SELECT"]

  depends_on = [postgresql_grant.zitadel_backup_usage]
}

resource "postgresql_grant" "zitadel_backup_select_sequences" {
  for_each = toset(local.zitadel_schemas)

  database    = postgresql_database.zitadel.name
  role        = postgresql_role.zitadel_backup.name
  schema      = each.value
  object_type = "sequence"
  privileges  = ["SELECT"]

  depends_on = [postgresql_grant.zitadel_backup_usage]
}

resource "postgresql_default_privileges" "zitadel_backup_future_tables" {
  for_each = toset(local.zitadel_schemas)

  database    = postgresql_database.zitadel.name
  owner       = postgresql_role.zitadel_admin.name
  role        = postgresql_role.zitadel_backup.name
  schema      = each.value
  object_type = "table"
  privileges  = ["SELECT"]

  depends_on = [postgresql_grant.zitadel_backup_usage]
}

resource "postgresql_default_privileges" "zitadel_backup_future_sequences" {
  for_each = toset(local.zitadel_schemas)

  database    = postgresql_database.zitadel.name
  owner       = postgresql_role.zitadel_admin.name
  role        = postgresql_role.zitadel_backup.name
  schema      = each.value
  object_type = "sequence"
  privileges  = ["SELECT"]

  depends_on = [postgresql_grant.zitadel_backup_usage]
}

resource "kubernetes_secret" "zitadel_backup_credentials" {
  metadata {
    name      = "zitadel-backup-credentials"
    namespace = kubernetes_namespace.infra.metadata[0].name
  }

  data = {
    username = postgresql_role.zitadel_backup.name
    password = random_password.zitadel_backup_password.result
  }

  lifecycle {
    ignore_changes = [data]
  }

  depends_on = [postgresql_role.zitadel_backup]
}

resource "random_password" "backup_encryption_key" {
  length  = 32
  special = false
}

resource "kubernetes_secret" "pg_backup_credentials" {
  metadata {
    name      = "pg-backup-credentials"
    namespace = kubernetes_namespace.infra.metadata[0].name
  }

  data = {
    username = postgresql_role.pg_backup.name
    password = random_password.pg_backup_password.result
  }

  lifecycle {
    ignore_changes = [data]
  }
}

# Separate secret for the backup encryption key — different access control domain from DB credentials.
# Mounted into the CronJob alongside pg-backup-credentials and zitadel-db-credentials.
resource "kubernetes_secret" "pg_backup_encryption_key" {
  metadata {
    name      = "pg-backup-encryption-key"
    namespace = kubernetes_namespace.infra.metadata[0].name
  }

  data = {
    encryption_key = random_password.backup_encryption_key.result
  }
}

resource "kubernetes_cron_job_v1" "pg_backup" {
  metadata {
    name      = "pg-backup"
    namespace = kubernetes_namespace.infra.metadata[0].name
  }

  spec {
    schedule                      = "0 2 * * *"
    concurrency_policy            = "Forbid"
    failed_jobs_history_limit     = 3
    successful_jobs_history_limit = 3

    job_template {
      metadata {
        labels = {
          "app" = "pg-backup"
        }
      }

      spec {
        active_deadline_seconds = 7200
        backoff_limit           = 2

        template {
          metadata {
            labels = {
              "app" = "pg-backup"
            }
          }

          spec {
            restart_policy = "OnFailure"
            node_selector  = var.node_selector

            security_context {
              seccomp_profile {
                type = "RuntimeDefault"
              }
            }

            image_pull_secrets {
              name = kubernetes_secret.ghcr_pull_infra.metadata[0].name
            }

            container {
              name  = "pg-backup"
              image = var.pg_backup_image

              command = ["/bin/sh", "-c"]
              args = [<<-EOT
                set -euo pipefail
                TIMESTAMP=$(date +%Y%m%d-%H%M%S)

                dump_db() {
                  local DB="$1"
                  local USER="$2"
                  local PASS="$3"
                  S3_KEY="backups/$${DB}/pgdump-$${DB}-$${TIMESTAMP}.sql.gz.enc"
                  S3_URI="s3://${var.backup_s3_bucket}/$${S3_KEY}"
                  echo "Dumping database: $${DB}"

                  # Stream directly to object storage: a postgres dump is much
                  # larger than a worker's ephemeral disk. pipefail makes any
                  # pg_dump/gzip/openssl/aws failure fail the Kubernetes Job.
                  PGPASSWORD="$${PASS}" pg_dump \
                    -h "$${PGHOST}" \
                    -U "$${USER}" \
                    -d "$${DB}" \
                    --no-owner \
                    --no-privileges \
                    | gzip \
                    | openssl enc -aes-256-cbc -salt -pbkdf2 \
                        -pass env:BACKUP_ENCRYPTION_KEY \
                    | aws s3 cp - "$${S3_URI}" \
                    --endpoint-url "${var.backup_s3_endpoint}"

                  REMOTE_SIZE=$(aws s3api head-object \
                    --bucket "${var.backup_s3_bucket}" \
                    --key "$${S3_KEY}" \
                    --endpoint-url "${var.backup_s3_endpoint}" \
                    --query ContentLength \
                    --output text)
                  if [ "$${REMOTE_SIZE}" -le 1024 ]; then
                    echo "Stored backup for $${DB} is implausibly small ($${REMOTE_SIZE} bytes)." >&2
                    return 1
                  fi
                  echo "Stored $${DB} backup ($${REMOTE_SIZE} bytes)."
                }

                # postgres DB: use pg_backup credentials (per-schema SELECT grants)
                dump_db "postgres" "$${PGUSER}" "$${PGPASSWORD}"

                # zitadel DB: use zitadel_backup credentials (SELECT-only, pg_backup has no access)
                dump_db "zitadel" "$${ZITADEL_PGUSER}" "$${ZITADEL_PGPASSWORD}"

                echo "Removing dumps older than 7 days..."
                CUTOFF=$(date -d @$(($(date +%s) - 604800)) +%Y%m%d)
                for DB in postgres zitadel; do
                  aws s3 ls "s3://${var.backup_s3_bucket}/backups/$${DB}/" \
                    --endpoint-url "${var.backup_s3_endpoint}" \
                    | awk '{print $4}' \
                    | while read -r key; do
                      FILE_DATE=$(echo "$${key}" | grep -oE '[0-9]{8}' | head -1)
                      if [ -n "$${FILE_DATE}" ] && [ "$${FILE_DATE}" -lt "$${CUTOFF}" ]; then
                        aws s3 rm "s3://${var.backup_s3_bucket}/backups/$${DB}/$${key}" \
                          --endpoint-url "${var.backup_s3_endpoint}"
                        echo "Deleted old backup: $${key}"
                      fi
                    done
                done

                echo "Key escrow: uploading recovery keys to infra-backup device..."
                echo -n "$${BACKUP_ENCRYPTION_KEY}" | \
                AWS_ACCESS_KEY_ID="$${ESCROW_ACCESS_KEY}" \
                AWS_SECRET_ACCESS_KEY="$${ESCROW_SECRET_KEY}" \
                aws s3 cp - "s3://${var.escrow_s3_bucket}/key-escrow/backup-encryption-key" \
                  --endpoint-url "${var.escrow_s3_endpoint}"
                echo -n "$${ZITADEL_MASTERKEY}" | \
                AWS_ACCESS_KEY_ID="$${ESCROW_ACCESS_KEY}" \
                AWS_SECRET_ACCESS_KEY="$${ESCROW_SECRET_KEY}" \
                aws s3 cp - "s3://${var.escrow_s3_bucket}/key-escrow/zitadel-masterkey" \
                  --endpoint-url "${var.escrow_s3_endpoint}"

                echo "Backup complete."
              EOT
              ]

              env {
                name  = "PGHOST"
                value = "postgresql-primary.infra.svc.cluster.local"
              }

              env {
                name = "PGUSER"
                value_from {
                  secret_key_ref {
                    name = kubernetes_secret.pg_backup_credentials.metadata[0].name
                    key  = "username"
                  }
                }
              }

              env {
                name = "PGPASSWORD"
                value_from {
                  secret_key_ref {
                    name = kubernetes_secret.pg_backup_credentials.metadata[0].name
                    key  = "password"
                  }
                }
              }

              env {
                name = "BACKUP_ENCRYPTION_KEY"
                value_from {
                  secret_key_ref {
                    name = kubernetes_secret.pg_backup_encryption_key.metadata[0].name
                    key  = "encryption_key"
                  }
                }
              }

              env {
                name = "ZITADEL_MASTERKEY"
                value_from {
                  secret_key_ref {
                    name = kubernetes_secret.zitadel_masterkey.metadata[0].name
                    key  = "masterkey"
                  }
                }
              }

              # Key escrow credentials (infra-backup Swiss Backup device, separate from db-backup)
              env {
                name = "ESCROW_ACCESS_KEY"
                value_from {
                  secret_key_ref {
                    name = kubernetes_secret.escrow_s3_credentials.metadata[0].name
                    key  = "access_key_id"
                  }
                }
              }

              env {
                name = "ESCROW_SECRET_KEY"
                value_from {
                  secret_key_ref {
                    name = kubernetes_secret.escrow_s3_credentials.metadata[0].name
                    key  = "secret_access_key"
                  }
                }
              }

              # Zitadel DB credentials — dedicated read-only backup role (not zitadel_admin)
              env {
                name = "ZITADEL_PGUSER"
                value_from {
                  secret_key_ref {
                    name = kubernetes_secret.zitadel_backup_credentials.metadata[0].name
                    key  = "username"
                  }
                }
              }

              env {
                name = "ZITADEL_PGPASSWORD"
                value_from {
                  secret_key_ref {
                    name = kubernetes_secret.zitadel_backup_credentials.metadata[0].name
                    key  = "password"
                  }
                }
              }

              env {
                name = "AWS_ACCESS_KEY_ID"
                value_from {
                  secret_key_ref {
                    name = "pg-backup-s3-credentials"
                    key  = "access_key_id"
                  }
                }
              }

              env {
                name = "AWS_SECRET_ACCESS_KEY"
                value_from {
                  secret_key_ref {
                    name = "pg-backup-s3-credentials"
                    key  = "secret_access_key"
                  }
                }
              }

              resources {
                requests = {
                  cpu    = "100m"
                  memory = "128Mi"
                }
                limits = {
                  cpu    = "500m"
                  memory = "512Mi"
                }
              }

              security_context {
                allow_privilege_escalation = false
                read_only_root_filesystem  = true
                run_as_non_root            = true
                run_as_user                = 65534
                capabilities {
                  drop = ["ALL"]
                }
              }

              volume_mount {
                name       = "tmp"
                mount_path = "/tmp"
              }
            }

            volume {
              name = "tmp"
              empty_dir {}
            }
          }
        }
      }
    }
  }

  depends_on = [
    helm_release.postgresql,
    kubernetes_secret.pg_backup_s3_credentials,
    kubernetes_secret.pg_backup_credentials,
    kubernetes_secret.pg_backup_encryption_key,
    kubernetes_secret.zitadel_backup_credentials,
    kubernetes_secret.zitadel_masterkey,
    kubernetes_secret.escrow_s3_credentials,
    kubernetes_network_policy.infra_allow_pg_backup_egress,
  ]
}

resource "kubernetes_secret" "pg_backup_s3_credentials" {
  metadata {
    name      = "pg-backup-s3-credentials"
    namespace = kubernetes_namespace.infra.metadata[0].name
  }

  data = {
    access_key_id     = var.backup_s3_access_key_id
    secret_access_key = var.backup_s3_secret_access_key
  }
}

resource "kubernetes_secret" "ghcr_pull_infra" {
  metadata {
    name      = "ghcr-pull"
    namespace = kubernetes_namespace.infra.metadata[0].name
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

resource "kubernetes_secret" "ghcr_pull_observability" {
  count = var.observability_enabled ? 1 : 0

  metadata {
    name      = "ghcr-pull"
    namespace = kubernetes_namespace.observability[0].metadata[0].name
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

resource "kubernetes_secret" "escrow_s3_credentials" {
  metadata {
    name      = "escrow-s3-credentials"
    namespace = kubernetes_namespace.infra.metadata[0].name
  }

  data = {
    access_key_id     = var.escrow_s3_access_key_id
    secret_access_key = var.escrow_s3_secret_access_key
  }
}

# =============================================================================
# PostgreSQL password rotation — manual, via Terraform
# =============================================================================
#
# There is no automated rotation CronJob (removed 2026-06-22: a standing
# exec-into-Postgres + patch-secrets-everywhere ServiceAccount, plus first-run
# footguns, wasn't worth it for the rotation cadence).
#
# ⚠️ A plain `tofu apply` does NOT rotate anything, and fails silently.
#
# Every credential resource here carries `lifecycle { ignore_changes = ... }` —
# on `password` for the roles and on `data` for the consumer secrets (see
# modules/tenant/main.tf and the resources below). Terraform will therefore
# neither ALTER a live role nor rewrite a secret whose value changed. You get a
# green apply, a clean plan, and an unrotated credential.
#
# The working procedure is in `.claude/skills/database/SKILL.md` ("Rotate a
# password"). In short: change it in PostgreSQL first (via psql `\password`,
# NOT `ALTER ROLE` — log_statement='ddl' would write the new password into the
# server log), update Keychain + the GitHub secret + Swiss Backup escrow, then
# force the consumer secret to be rewritten with an explicit
# `-replace=<address of the kubernetes_secret>`. Finally restart the consumers:
# DATABASE_URL is injected as a secretKeyRef env var and is snapshotted at pod
# start.
#
# An earlier version of this comment claimed a plain apply was sufficient. It
# was written when the rotation CronJob was removed (2026-06-22) and did not
# account for the ignore_changes blocks, which were already in place.

# =============================================================================
# ingress-nginx
# =============================================================================
resource "helm_release" "ingress_nginx" {
  name             = "ingress-nginx"
  repository       = "https://kubernetes.github.io/ingress-nginx"
  chart            = "ingress-nginx"
  version          = "4.15.1"
  namespace        = kubernetes_namespace.infra.metadata[0].name
  disable_webhooks = true

  values = [lookup(var.helm_values, "ingress-nginx", file("${path.module}/../../../helm/values/ingress-nginx.yaml"))]

  depends_on = [
  ]
}

# =============================================================================
# cert-manager
# =============================================================================
resource "helm_release" "cert_manager" {
  name       = "cert-manager"
  repository = "https://charts.jetstack.io"
  chart      = "cert-manager"
  version    = "v1.20.2"
  namespace  = kubernetes_namespace.infra.metadata[0].name

  values = [lookup(var.helm_values, "cert-manager", file("${path.module}/../../../helm/values/cert-manager.yaml"))]

  depends_on = [
  ]
}

# ClusterIssuer for Let's Encrypt production certificates.
# Uses local_file + null_resource + kubectl because kubernetes_manifest
# requires CRDs at plan time, but cert-manager CRDs only exist after
# helm_release.cert_manager. The template file approach eliminates shell
# injection risk from variable interpolation.
resource "local_file" "letsencrypt_issuer_manifest" {
  content = templatefile("${path.module}/templates/clusterissuer-letsencrypt.yaml.tpl", {
    email = var.letsencrypt_email
  })
  filename = var.issuer_manifest_path
}

resource "null_resource" "letsencrypt_issuer" {
  triggers = {
    manifest_sha = local_file.letsencrypt_issuer_manifest.content_sha256
  }

  provisioner "local-exec" {
    command = "kubectl --kubeconfig \"${var.kubeconfig_path}\" apply -f \"${local_file.letsencrypt_issuer_manifest.filename}\""
  }

  depends_on = [
    helm_release.cert_manager,
    helm_release.ingress_nginx,
    local_file.letsencrypt_issuer_manifest,
  ]
}

# =============================================================================
# Zitadel — shared identity provider (OIDC/OAuth2)
# =============================================================================
#
# Resources provisioned by this section:
#   "postgresql_role.zitadel_admin"                            — PG role for migrations/setup (CREATEDB)
#   "postgresql_role.zitadel"                                  — PG role for runtime queries (no CREATEDB)
#   "postgresql_database.zitadel"                              — dedicated database (Zitadel manages its own schemas)
#   "kubernetes_secret.zitadel_masterkey"                      — AES encryption key for data at rest
#   "helm_release.zitadel"                                     — Zitadel chart deployment
#   "kubernetes_ingress_v1.zitadel"                            — ingress for auth.<domain>
#   "kubernetes_network_policy.infra_allow_tenant_to_zitadel"  — tenant → Zitadel:8080
#   "postgresql_grant.zitadel_revoke_public_connect"            — revokes PUBLIC connect on zitadel DB
#   "kubernetes_secret.zitadel_db_credentials"                 — PG credentials for debug/future use
#   "kubernetes_network_policy.infra_allow_ingress_to_zitadel" — ingress-nginx → Zitadel:8080

# --- PostgreSQL: dedicated database and role ---
# Zitadel requires a dedicated database (not just a schema) because its internal
# schema management (event sourcing, projections) expects database-level ownership.
resource "postgresql_role" "zitadel_admin" {
  name            = "zitadel_admin"
  login           = true
  password        = var.zitadel_admin_db_password
  create_database = true
  create_role     = true

  lifecycle {
    ignore_changes = [password]
  }

  depends_on = [helm_release.postgresql]
}

resource "postgresql_role" "zitadel" {
  name     = "zitadel"
  login    = true
  password = var.zitadel_db_password

  lifecycle {
    ignore_changes = [password]
  }

  depends_on = [helm_release.postgresql]
}

resource "postgresql_database" "zitadel" {
  name  = "zitadel"
  owner = postgresql_role.zitadel_admin.name

  depends_on = [postgresql_role.zitadel_admin]
}

resource "postgresql_extension" "amcheck_zitadel" {
  name     = "amcheck"
  database = postgresql_database.zitadel.name

  depends_on = [postgresql_database.zitadel]
}

resource "postgresql_extension" "pgstattuple_zitadel" {
  name     = "pgstattuple"
  database = postgresql_database.zitadel.name

  depends_on = [postgresql_database.zitadel]
}

# Revoke default PUBLIC connect on the zitadel database.
# PostgreSQL grants CONNECT to PUBLIC on all databases by default.
# Without this, any PG role (e.g. tenant roles) can connect to the zitadel database.
resource "postgresql_grant" "zitadel_revoke_public_connect" {
  database    = postgresql_database.zitadel.name
  role        = "public"
  object_type = "database"
  privileges  = []

  depends_on = [postgresql_database.zitadel]
}

# --- Kubernetes secrets ---
resource "kubernetes_secret" "zitadel_masterkey" {
  metadata {
    name      = "zitadel-masterkey"
    namespace = kubernetes_namespace.infra.metadata[0].name
  }

  data = {
    masterkey = var.zitadel_masterkey
  }
}

resource "kubernetes_secret" "zitadel_db_credentials" {
  metadata {
    name      = "zitadel-db-credentials"
    namespace = kubernetes_namespace.infra.metadata[0].name
  }

  data = {
    username       = postgresql_role.zitadel.name
    password       = var.zitadel_db_password
    admin_username = postgresql_role.zitadel_admin.name
    admin_password = var.zitadel_admin_db_password
  }

  lifecycle {
    ignore_changes = [data]
  }

  depends_on = [postgresql_role.zitadel, postgresql_role.zitadel_admin]
}

# --- Network policies ---

# Allow tenant pods to reach Zitadel (token validation, JWKS, userinfo)
resource "kubernetes_network_policy" "infra_allow_tenant_to_zitadel" {
  metadata {
    name      = "allow-tenant-to-zitadel"
    namespace = kubernetes_namespace.infra.metadata[0].name
  }

  spec {
    pod_selector {
      match_labels = {
        "app.kubernetes.io/name" = "zitadel"
      }
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
  }
}

# Allow ingress-nginx to reach Zitadel (Zitadel runs in infra namespace, same as nginx)
resource "kubernetes_network_policy" "infra_allow_ingress_to_zitadel" {
  metadata {
    name      = "allow-ingress-to-zitadel"
    namespace = kubernetes_namespace.infra.metadata[0].name
  }

  spec {
    pod_selector {
      match_labels = {
        "app.kubernetes.io/name" = "zitadel"
      }
    }

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

      # The Zitadel v2 login UI (separate deployment) calls the Zitadel API.
      from {
        pod_selector {
          match_labels = {
            "app.kubernetes.io/name" = "zitadel-login"
          }
        }
      }

      # Zitadel exposes its standard /debug/metrics endpoint on the same port.
      # Collectors carry no Zitadel credential and retain only approved routes.
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
        port     = "8080"
      }
    }
  }
}

# Egress for the Zitadel v2 login UI -> the Zitadel API (8080). Without this the
# login pod's init probe to http://zitadel:8080/debug/ready times out and it
# crash-loops. DNS + K8s API egress are covered by the shared infra policies.
resource "kubernetes_network_policy" "infra_allow_zitadel_login_egress" {
  metadata {
    name      = "allow-zitadel-login-egress"
    namespace = kubernetes_namespace.infra.metadata[0].name
  }

  spec {
    pod_selector {
      match_labels = {
        "app.kubernetes.io/name" = "zitadel-login"
      }
    }

    policy_types = ["Egress"]

    egress {
      to {
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

# Allow the ingress-nginx controller to reach the Zitadel v2 login UI on 3000.
resource "kubernetes_network_policy" "infra_allow_ingress_to_zitadel_login" {
  metadata {
    name      = "allow-ingress-to-zitadel-login"
    namespace = kubernetes_namespace.infra.metadata[0].name
  }

  spec {
    pod_selector {
      match_labels = {
        "app.kubernetes.io/name" = "zitadel-login"
      }
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
        port     = "3000"
      }
    }
  }
}

# --- Helm release ---
resource "helm_release" "zitadel" {
  name       = "zitadel"
  repository = "https://charts.zitadel.com"
  chart      = "zitadel"
  version    = "10.0.4"
  namespace  = kubernetes_namespace.infra.metadata[0].name
  timeout    = 600
  wait       = false
  # Hooks must run: Zitadel ships init (migrations) + setup (FirstInstance) as
  # pre-install/pre-upgrade Helm hooks. With these disabled, setup never runs and
  # the instance can never (re)initialize.
  disable_webhooks = false

  values = [lookup(var.helm_values, "zitadel", file("${path.module}/../../../helm/values/zitadel.yaml"))]

  # External domain (used in OIDC issuer URL — must match DNS)
  set {
    name  = "zitadel.configmapConfig.ExternalDomain"
    value = var.identity_domain
  }

  # First-instance admin. Only applied by `zitadel setup` against an EMPTY
  # database (i.e. on a fresh init). Password is an infra credential (Keychain
  # / escrow / GitHub secret), injected sensitively below.
  set {
    name  = "zitadel.configmapConfig.FirstInstance.Org.Name"
    value = var.identity_admin.organization
  }
  set {
    name  = "zitadel.configmapConfig.FirstInstance.Org.Human.UserName"
    value = var.identity_admin.email
  }
  set {
    name  = "zitadel.configmapConfig.FirstInstance.Org.Human.FirstName"
    value = var.identity_admin.first_name
  }
  set {
    name  = "zitadel.configmapConfig.FirstInstance.Org.Human.LastName"
    value = var.identity_admin.last_name
  }
  set {
    name  = "zitadel.configmapConfig.FirstInstance.Org.Human.Email.Address"
    value = var.identity_admin.email
  }
  set {
    name  = "zitadel.configmapConfig.FirstInstance.Org.Human.Email.Verified"
    value = "true"
  }
  set {
    name  = "zitadel.configmapConfig.FirstInstance.Org.Human.PasswordChangeRequired"
    value = "false"
  }
  set_sensitive {
    name  = "zitadel.secretConfig.FirstInstance.Org.Human.Password"
    value = var.zitadel_admin_password
  }

  # Database password (sensitive — not in values file)
  set_sensitive {
    name  = "zitadel.secretConfig.Database.Postgres.User.Password"
    value = var.zitadel_db_password
  }

  set_sensitive {
    name  = "zitadel.secretConfig.Database.Postgres.Admin.Password"
    value = var.zitadel_admin_db_password
  }

  depends_on = [
    kubernetes_secret.zitadel_masterkey,
    postgresql_database.zitadel,
    helm_release.postgresql,
  ]
}

# --- Ingress ---
resource "kubernetes_ingress_v1" "zitadel" {
  metadata {
    name      = "zitadel"
    namespace = kubernetes_namespace.infra.metadata[0].name

    # No cert-manager / TLS secret: TLS is terminated at the Cloudflare edge
    # (Universal SSL); the request reaches nginx over the tunnel as HTTP.
    annotations = {
      "nginx.ingress.kubernetes.io/limit-rps"              = "5"
      "nginx.ingress.kubernetes.io/limit-burst-multiplier" = "3"
      "nginx.ingress.kubernetes.io/limit-connections"      = "10"
    }
  }

  spec {
    ingress_class_name = "nginx"

    rule {
      host = var.identity_domain

      http {
        # Zitadel v2 login UI is a separate service (zitadel-login:3000).
        path {
          path      = "/ui/v2/login"
          path_type = "Prefix"

          backend {
            service {
              name = "zitadel-login"
              port {
                number = 3000
              }
            }
          }
        }

        # Everything else -> the Zitadel API / console.
        path {
          path      = "/"
          path_type = "Prefix"

          backend {
            service {
              name = "zitadel"
              port {
                number = 8080
              }
            }
          }
        }
      }
    }
  }

  depends_on = [
    helm_release.zitadel,
    helm_release.ingress_nginx,
  ]
}

# @lore A grant inherited from PostgreSQL's PUBLIC pseudo-role cannot be
# cancelled by revoking the same privilege from an individual tenant role.
# Own the shared public-schema boundary once, before any tenant can inherit it.
resource "postgresql_grant" "revoke_public_schema" {
  database    = "postgres"
  role        = "public"
  schema      = "public"
  object_type = "schema"
  privileges  = []

  depends_on = [helm_release.postgresql]
}

# =============================================================================
# Storage broker (credential mediation + tenant/workspace row authorization)
# =============================================================================
#
# Holds the ONLY S3 credential. Governed v2 requests resolve opaque object rows
# with tenant plus optional exact-workspace scope before returning short-TTL,
# single-object transfers. The deployed v1 prefix API remains during migration.
# See docs/concepts/governed-object-storage.md.
#
# Dedicated, project-scoped S3 credential — separate from the per-app
# object-storage credentials so it rotates independently (taint + apply).
# Infomaniak credentials are project-scoped regardless; the isolation guarantee
# comes from the broker, not the credential.
resource "random_password" "storage_broker_database" {
  length  = 32
  special = false
}

resource "postgresql_role" "storage_broker" {
  name     = "storage_broker"
  login    = true
  password = random_password.storage_broker_database.result

  lifecycle {
    ignore_changes = [password]
  }

  depends_on = [helm_release.postgresql]
}

resource "postgresql_schema" "storage_broker" {
  name  = "storage_broker"
  owner = postgresql_role.storage_broker.name

  depends_on = [postgresql_role.storage_broker]
}

resource "postgresql_grant" "storage_broker_connect" {
  database    = "postgres"
  role        = postgresql_role.storage_broker.name
  object_type = "database"
  privileges  = ["CONNECT"]

  depends_on = [postgresql_role.storage_broker]
}

resource "postgresql_grant" "storage_broker_schema" {
  database    = "postgres"
  role        = postgresql_role.storage_broker.name
  schema      = postgresql_schema.storage_broker.name
  object_type = "schema"
  privileges  = ["CREATE", "USAGE"]

  depends_on = [postgresql_schema.storage_broker]
}

# Governed object and execution-grant rows are authorization state and must be
# restored with the rest of the platform database. The backup role receives no
# write access and the infra console deliberately receives no access at all.
resource "postgresql_grant" "pg_backup_usage_storage_broker" {
  database    = "postgres"
  role        = postgresql_role.pg_backup.name
  schema      = postgresql_schema.storage_broker.name
  object_type = "schema"
  privileges  = ["USAGE"]

  # PostgreSQL's provider revokes/regrants privileges. Serialize this after the
  # owner grant to avoid concurrent pg_namespace ACL updates on first apply.
  depends_on = [postgresql_grant.storage_broker_schema, postgresql_role.pg_backup]
}

resource "postgresql_grant" "pg_backup_select_storage_broker" {
  database    = "postgres"
  role        = postgresql_role.pg_backup.name
  schema      = postgresql_schema.storage_broker.name
  object_type = "table"
  privileges  = ["SELECT"]

  depends_on = [postgresql_grant.pg_backup_usage_storage_broker]
}

resource "postgresql_default_privileges" "pg_backup_future_storage_broker_tables" {
  database    = "postgres"
  owner       = postgresql_role.storage_broker.name
  role        = postgresql_role.pg_backup.name
  schema      = postgresql_schema.storage_broker.name
  object_type = "table"
  privileges  = ["SELECT"]

  depends_on = [postgresql_grant.pg_backup_usage_storage_broker]
}

resource "openstack_identity_ec2_credential_v3" "storage_broker" {}

# -----------------------------------------------------------------------------
# infra-console — read-only console and MCP surface (docs/plans/infra-console.md)
#
# count = 0 until an image exists, so the platform applies cleanly before the
# first build. The console holds no state and writes nothing anywhere; removing
# it leaves every source exactly as it was.
# -----------------------------------------------------------------------------
# The console's own S3 credential. Separate from the broker's so either can be
# revoked without taking the other down.
resource "openstack_identity_ec2_credential_v3" "infra_console" {
  # Gated with the module: without this a live S3 credential is minted and
  # stored in state on the first apply even while the console is count = 0.
  count = var.infra_console_image == "" ? 0 : 1
}

# Swift exposes exact object count and byte totals as container metadata. The
# S3 credential above cannot be exchanged for a Swift token on Infomaniak, so
# use a separately revocable application credential. SwiftOperator is the only
# provider role that can read container metadata; the console code performs a
# single HEAD and never lists or writes objects.
resource "openstack_identity_application_credential_v3" "infra_console_swift" {
  count = var.infra_console_image == "" ? 0 : 1

  name         = "infra-console-storage-metadata"
  description  = "Exact container counters for the infra console"
  roles        = ["SwiftOperator"]
  unrestricted = false
}

resource "kubernetes_pod_disruption_budget_v1" "zitadel" {
  metadata {
    name      = "zitadel"
    namespace = kubernetes_namespace.infra.metadata[0].name
  }

  spec {
    min_available = "1"

    selector {
      match_labels = {
        "app.kubernetes.io/name" = "zitadel"
      }
    }
  }

  depends_on = [helm_release.zitadel]
}

# =============================================================================
# ValidatingAdmissionPolicy — restrict Ingress hostnames per tenant namespace
# =============================================================================
#
# Enforces: any Ingress created in a tenant namespace (platform.infra/tenant=true)
# may only use hosts permitted by that namespace's platform.infra/allowed-hosts
# annotation (set by the tenant module from var.allowed_ingress_domains). A host
# is permitted if it equals an allowed domain or is a subdomain of one — so
# "hyperidle.com" permits hyperidle.com, www.hyperidle.com, beta.hyperidle.com.
#
# FAIL-CLOSED: if the annotation is missing/empty, or a host matches none of the
# allowed domains, the Ingress is denied. This both supports the beta.<domain>
# and main-<domain> schemes and prevents a tenant from claiming another tenant's
# or infra's hostnames (e.g. id.nimmly.io).
#
# Requires Kubernetes >= 1.30 (ValidatingAdmissionPolicy GA). Uses
# kubernetes_manifest (no native provider resource type).
resource "kubernetes_manifest" "ingress_hostname_policy" {
  manifest = yamldecode(<<-YAML
    apiVersion: admissionregistration.k8s.io/v1
    kind: ValidatingAdmissionPolicy
    metadata:
      name: tenant-ingress-hostname
    spec:
      failurePolicy: Fail
      matchConstraints:
        resourceRules:
          - apiGroups:   ["networking.k8s.io"]
            apiVersions: ["v1"]
            resources:   ["ingresses"]
            operations:  ["CREATE", "UPDATE"]
      variables:
        - name: allowed
          expression: >
            has(namespaceObject.metadata.annotations) &&
            'platform.infra/allowed-hosts' in namespaceObject.metadata.annotations &&
            size(namespaceObject.metadata.annotations['platform.infra/allowed-hosts']) > 0
              ? namespaceObject.metadata.annotations['platform.infra/allowed-hosts'].split(',')
              : []
      validations:
        - expression: >
            has(object.spec.rules) &&
            object.spec.rules.all(rule,
              has(rule.host) &&
              variables.allowed.exists(d, rule.host == d || rule.host.endsWith('.' + d))
            )
          message: "Ingress host not permitted: must be (a subdomain of) a domain in the namespace's platform.infra/allowed-hosts annotation"
        - expression: >
            !has(object.spec.tls) ||
            object.spec.tls.all(tls,
              !has(tls.hosts) ||
              tls.hosts.all(h,
                variables.allowed.exists(d, h == d || h.endsWith('.' + d))
              )
            )
          message: "Ingress TLS host not permitted by the namespace's platform.infra/allowed-hosts annotation"
  YAML
  )
}

resource "kubernetes_manifest" "ingress_hostname_policy_binding" {
  manifest = yamldecode(<<-YAML
    apiVersion: admissionregistration.k8s.io/v1
    kind: ValidatingAdmissionPolicyBinding
    metadata:
      name: tenant-ingress-hostname
    spec:
      policyName: tenant-ingress-hostname
      validationActions: ["Deny"]
      matchResources:
        namespaceSelector:
          matchLabels:
            platform.infra/tenant: "true"
  YAML
  )

  depends_on = [kubernetes_manifest.ingress_hostname_policy]
}
