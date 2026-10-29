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
  collector_name = "observability-collector"
  compactor_name = "telemetry-data"
  common_labels = {
    "app.kubernetes.io/part-of"    = "platform-observability"
    "app.kubernetes.io/managed-by" = "opentofu"
  }
  collector_labels = merge(local.common_labels, {
    "app.kubernetes.io/name"      = local.collector_name
    "app.kubernetes.io/component" = "collector"
  })
  compactor_labels = merge(local.common_labels, {
    "app.kubernetes.io/name"      = local.compactor_name
    "app.kubernetes.io/component" = "compactor"
  })
}

# Infomaniak EC2 credentials are project-wide, not bucket-scoped. This one is
# dedicated so telemetry access can be rotated independently and is exposed
# only to the collector and compactor pods in the infra namespace.
resource "openstack_objectstorage_container_v1" "telemetry" {
  name   = var.bucket_name
  region = var.object_storage_region

  metadata = {
    purpose    = "platform-observability"
    managed-by = "opentofu"
  }
}

resource "openstack_identity_ec2_credential_v3" "telemetry" {}

resource "kubernetes_secret" "s3" {
  metadata {
    name      = "observability-s3"
    namespace = var.namespace
    labels    = local.common_labels
  }

  data = {
    S3_ENDPOINT          = var.s3_endpoint
    S3_REGION            = var.s3_region
    S3_BUCKET            = openstack_objectstorage_container_v1.telemetry.name
    S3_ACCESS_KEY_ID     = openstack_identity_ec2_credential_v3.telemetry.access
    S3_SECRET_ACCESS_KEY = openstack_identity_ec2_credential_v3.telemetry.secret
  }

  type = "Opaque"
}

resource "kubernetes_config_map" "collector" {
  metadata {
    name      = "observability-collector-config"
    namespace = var.namespace
    labels    = local.collector_labels
  }

  data = {
    "config.yaml" = var.collector_config
  }
}

resource "kubernetes_service_account" "collector" {
  metadata {
    name      = local.collector_name
    namespace = var.namespace
    labels    = local.collector_labels
  }

  automount_service_account_token = true
}

resource "kubernetes_cluster_role" "collector" {
  metadata {
    name   = local.collector_name
    labels = local.collector_labels
  }

  rule {
    api_groups = [""]
    resources = [
      "events",
      "namespaces",
      "nodes",
      "persistentvolumeclaims",
      "persistentvolumes",
      "pods",
      "replicationcontrollers",
      "resourcequotas",
      "services",
    ]
    verbs = ["get", "list", "watch"]
  }

  rule {
    api_groups = [""]
    resources  = ["nodes/stats"]
    verbs      = ["get"]
  }

  rule {
    api_groups = ["apps"]
    resources  = ["daemonsets", "deployments", "replicasets", "statefulsets"]
    verbs      = ["get", "list", "watch"]
  }

  rule {
    api_groups = ["batch"]
    resources  = ["cronjobs", "jobs"]
    verbs      = ["get", "list", "watch"]
  }

  rule {
    api_groups = ["autoscaling"]
    resources  = ["horizontalpodautoscalers"]
    verbs      = ["get", "list", "watch"]
  }

  rule {
    api_groups = ["events.k8s.io"]
    resources  = ["events"]
    verbs      = ["get", "list", "watch"]
  }

  rule {
    api_groups = ["storage.k8s.io"]
    resources  = ["storageclasses"]
    verbs      = ["get", "list", "watch"]
  }
}

resource "kubernetes_cluster_role_binding" "collector" {
  metadata {
    name   = local.collector_name
    labels = local.collector_labels
  }

  role_ref {
    api_group = "rbac.authorization.k8s.io"
    kind      = "ClusterRole"
    name      = kubernetes_cluster_role.collector.metadata[0].name
  }

  subject {
    kind      = "ServiceAccount"
    name      = kubernetes_service_account.collector.metadata[0].name
    namespace = var.namespace
  }
}

resource "kubernetes_role" "collector_lease" {
  metadata {
    name      = "${local.collector_name}-lease"
    namespace = var.namespace
    labels    = local.collector_labels
  }

  rule {
    api_groups     = ["coordination.k8s.io"]
    resources      = ["leases"]
    resource_names = [local.collector_name]
    verbs          = ["get", "update", "patch", "delete"]
  }

  rule {
    api_groups = ["coordination.k8s.io"]
    resources  = ["leases"]
    verbs      = ["create"]
  }
}

resource "kubernetes_role_binding" "collector_lease" {
  metadata {
    name      = "${local.collector_name}-lease"
    namespace = var.namespace
    labels    = local.collector_labels
  }

  role_ref {
    api_group = "rbac.authorization.k8s.io"
    kind      = "Role"
    name      = kubernetes_role.collector_lease.metadata[0].name
  }

  subject {
    kind      = "ServiceAccount"
    name      = kubernetes_service_account.collector.metadata[0].name
    namespace = var.namespace
  }
}

resource "kubernetes_service_v1" "collector" {
  metadata {
    name      = local.collector_name
    namespace = var.namespace
    labels    = local.collector_labels
  }

  spec {
    selector                = { "app.kubernetes.io/name" = local.collector_name }
    internal_traffic_policy = "Local"

    port {
      name        = "otlp-grpc"
      port        = 4317
      target_port = "otlp-grpc"
      protocol    = "TCP"
    }

    port {
      name        = "otlp-http"
      port        = 4318
      target_port = "otlp-http"
      protocol    = "TCP"
    }
  }
}

resource "kubernetes_daemon_set_v1" "collector" {
  metadata {
    name      = local.collector_name
    namespace = var.namespace
    labels    = local.collector_labels
  }

  depends_on = [
    kubernetes_cluster_role_binding.collector,
    kubernetes_role_binding.collector_lease,
    kubernetes_network_policy.collector_egress,
    kubernetes_manifest.collector_kubelet_egress,
    kubernetes_network_policy.collector_ingress,
  ]

  spec {
    selector {
      match_labels = { "app.kubernetes.io/name" = local.collector_name }
    }

    strategy {
      type = "RollingUpdate"
      rolling_update {
        max_unavailable = "1"
      }
    }

    template {
      metadata {
        labels = local.collector_labels
        annotations = {
          "checksum/config" = sha256(var.collector_config)
        }
      }

      spec {
        service_account_name             = kubernetes_service_account.collector.metadata[0].name
        automount_service_account_token  = true
        termination_grace_period_seconds = 30

        image_pull_secrets {
          name = var.ghcr_pull_secret_name
        }

        security_context {
          # Container log files are root-readable on the nodes. The process has
          # no capabilities, no writable root filesystem, and the host mount is
          # read-only; root is used only to read the allowlisted log path.
          run_as_user  = 0
          run_as_group = 0
          seccomp_profile {
            type = "RuntimeDefault"
          }
        }

        container {
          name              = "collector"
          image             = var.collector_image
          image_pull_policy = "IfNotPresent"
          args              = ["--config=/etc/otelcol/config.yaml"]

          port {
            name           = "otlp-grpc"
            container_port = 4317
          }
          port {
            name           = "otlp-http"
            container_port = 4318
          }
          port {
            name           = "health"
            container_port = 13133
          }
          env {
            name = "K8S_NODE_NAME"
            value_from {
              field_ref {
                field_path = "spec.nodeName"
              }
            }
          }
          env {
            name = "K8S_NODE_IP"
            value_from {
              field_ref {
                field_path = "status.hostIP"
              }
            }
          }

          # The local PutObject transport honors these settings and sends a
          # regular content-length request accepted by Infomaniak's endpoint.
          env {
            name  = "AWS_REQUEST_CHECKSUM_CALCULATION"
            value = "WHEN_REQUIRED"
          }
          env {
            name  = "AWS_RESPONSE_CHECKSUM_VALIDATION"
            value = "WHEN_REQUIRED"
          }

          dynamic "env" {
            for_each = toset(["S3_ENDPOINT", "S3_REGION", "S3_BUCKET"])
            content {
              name = env.value
              value_from {
                secret_key_ref {
                  name = kubernetes_secret.s3.metadata[0].name
                  key  = env.value
                }
              }
            }
          }

          # The Collector's AWS exporter uses the standard AWS SDK credential
          # chain. The compactor below deliberately keeps its S3_* names.
          env {
            name = "AWS_ACCESS_KEY_ID"
            value_from {
              secret_key_ref {
                name = kubernetes_secret.s3.metadata[0].name
                key  = "S3_ACCESS_KEY_ID"
              }
            }
          }
          env {
            name = "AWS_SECRET_ACCESS_KEY"
            value_from {
              secret_key_ref {
                name = kubernetes_secret.s3.metadata[0].name
                key  = "S3_SECRET_ACCESS_KEY"
              }
            }
          }

          resources {
            requests = var.collector_resources.requests
            limits   = var.collector_resources.limits
          }

          security_context {
            allow_privilege_escalation = false
            read_only_root_filesystem  = true
            run_as_user                = 0
            capabilities {
              drop = ["ALL"]
            }
          }

          readiness_probe {
            http_get {
              path = "/"
              port = 13133
            }
            initial_delay_seconds = 3
            period_seconds        = 10
          }

          liveness_probe {
            http_get {
              path = "/"
              port = 13133
            }
            initial_delay_seconds = 10
            period_seconds        = 20
          }

          volume_mount {
            name       = "config"
            mount_path = "/etc/otelcol"
            read_only  = true
          }
          volume_mount {
            name       = "pod-logs"
            mount_path = "/var/log/pods"
            read_only  = true
          }
          volume_mount {
            name       = "state"
            mount_path = "/var/lib/otelcol"
          }
        }

        volume {
          name = "config"
          config_map {
            name = kubernetes_config_map.collector.metadata[0].name
          }
        }
        volume {
          name = "pod-logs"
          host_path {
            path = "/var/log/pods"
            type = "Directory"
          }
        }
        volume {
          name = "state"
          empty_dir {
            size_limit = "256Mi"
          }
        }
      }
    }
  }
}

resource "kubernetes_cron_job_v1" "compactor" {
  metadata {
    name      = local.compactor_name
    namespace = var.namespace
    labels    = local.compactor_labels
  }

  spec {
    schedule                      = var.compaction_schedule
    concurrency_policy            = "Forbid"
    failed_jobs_history_limit     = 3
    successful_jobs_history_limit = 1
    starting_deadline_seconds     = 120

    job_template {
      metadata {
        labels = local.compactor_labels
      }

      spec {
        backoff_limit              = 2
        ttl_seconds_after_finished = 900

        template {
          metadata {
            labels = local.compactor_labels
          }

          spec {
            restart_policy                  = "OnFailure"
            automount_service_account_token = false

            image_pull_secrets {
              name = var.ghcr_pull_secret_name
            }

            security_context {
              run_as_non_root = true
              run_as_user     = 65532
              run_as_group    = 65532
              seccomp_profile {
                type = "RuntimeDefault"
              }
            }

            container {
              name              = "compactor"
              image             = var.compactor_image
              image_pull_policy = "IfNotPresent"
              args              = ["compact"]

              dynamic "env" {
                for_each = toset(["S3_ENDPOINT", "S3_REGION", "S3_BUCKET", "S3_ACCESS_KEY_ID", "S3_SECRET_ACCESS_KEY"])
                content {
                  name = env.value
                  value_from {
                    secret_key_ref {
                      name = kubernetes_secret.s3.metadata[0].name
                      key  = env.value
                    }
                  }
                }
              }

              env {
                name  = "COMPACTION_SEAL_DELAY_SECONDS"
                value = "120"
              }
              env {
                name  = "APPLICATION_TELEMETRY_SOURCES_JSON"
                value = jsonencode(var.application_telemetry_sources)
              }
              env {
                name  = "RAW_RETENTION_SECONDS"
                value = tostring(var.raw_retention_seconds)
              }

              resources {
                requests = var.compactor_resources.requests
                limits   = var.compactor_resources.limits
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
            }
          }
        }
      }
    }
  }

  depends_on = [kubernetes_network_policy.compactor_egress]
}

resource "kubernetes_network_policy" "collector_ingress" {
  metadata {
    name      = "allow-observability-collector-ingress"
    namespace = var.namespace
    labels    = local.collector_labels
  }

  spec {
    pod_selector {
      match_labels = { "app.kubernetes.io/name" = local.collector_name }
    }
    policy_types = ["Ingress"]

    ingress {
      dynamic "from" {
        for_each = var.otlp_ingress_namespaces
        content {
          namespace_selector {
            match_labels = { "kubernetes.io/metadata.name" = from.value }
          }
        }
      }
      ports {
        protocol = "TCP"
        port     = "4317"
      }
      ports {
        protocol = "TCP"
        port     = "4318"
      }
    }
  }
}

resource "kubernetes_network_policy" "collector_egress" {
  metadata {
    name      = "allow-observability-collector-egress"
    namespace = var.namespace
    labels    = local.collector_labels
  }

  spec {
    pod_selector {
      match_labels = { "app.kubernetes.io/name" = local.collector_name }
    }
    policy_types = ["Egress"]

    # The in-cluster Kubernetes Service is rewritten to this managed control
    # plane endpoint before NetworkPolicy enforcement.
    egress {
      to {
        ip_block { cidr = var.apiserver_cidr }
      }
      ports {
        protocol = "TCP"
        port     = var.apiserver_port
      }
    }

    # DNS for the public S3 endpoint.
    egress {
      to {
        namespace_selector {
          match_labels = { "kubernetes.io/metadata.name" = "kube-system" }
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

    # Node-local collectors scrape only PostgreSQL's existing exporter port.
    egress {
      to {
        namespace_selector {
          match_labels = { "kubernetes.io/metadata.name" = "infra" }
        }
        pod_selector {
          match_labels = { "app.kubernetes.io/name" = "postgresql" }
        }
      }
      ports {
        protocol = "TCP"
        port     = "9187"
      }
    }

    # Storage-broker metrics use a separate listener that no tenant can reach.
    egress {
      to {
        namespace_selector {
          match_labels = { "kubernetes.io/metadata.name" = "infra" }
        }
        pod_selector {
          match_labels = { "app.kubernetes.io/name" = "storage-broker" }
        }
      }
      ports {
        protocol = "TCP"
        port     = "9090"
      }
    }

    # Zitadel's standard /debug/metrics endpoint shares its existing service
    # port. The collector has no Zitadel credential and retains only a bounded
    # auth-route allowlist from the response.
    egress {
      to {
        namespace_selector {
          match_labels = { "kubernetes.io/metadata.name" = "infra" }
        }
        pod_selector {
          match_labels = { "app.kubernetes.io/name" = "zitadel" }
        }
      }
      ports {
        protocol = "TCP"
        port     = "8080"
      }
    }

    # Edge metrics: the ingress-nginx controller's :10254 endpoint and
    # cloudflared's fixed internal :2000 metrics port.
    egress {
      to {
        namespace_selector {
          match_labels = { "kubernetes.io/metadata.name" = "infra" }
        }
        pod_selector {
          match_labels = {
            "app.kubernetes.io/name"      = "ingress-nginx"
            "app.kubernetes.io/component" = "controller"
          }
        }
      }
      ports {
        protocol = "TCP"
        port     = "10254"
      }
    }

    egress {
      to {
        namespace_selector {
          match_labels = { "kubernetes.io/metadata.name" = "infra" }
        }
        pod_selector {
          match_labels = { "app.kubernetes.io/name" = "cloudflared" }
        }
      }
      ports {
        protocol = "TCP"
        port     = "2000"
      }
    }

    # Direct writes to the public Infomaniak S3 endpoint.
    egress {
      to {
        ip_block {
          cidr   = "0.0.0.0/0"
          except = ["10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"]
        }
      }
      ports {
        protocol = "TCP"
        port     = "443"
      }
    }
  }
}

# Cilium assigns node addresses its reserved host identity, so Kubernetes
# ipBlock rules deliberately do not match them. Allow only this DaemonSet to
# reach the kubelet on its local node; this avoids host networking and the
# broad nodes/proxy permission required by API-server proxying.
resource "kubernetes_manifest" "collector_kubelet_egress" {
  manifest = {
    apiVersion = "cilium.io/v2"
    kind       = "CiliumNetworkPolicy"
    metadata = {
      name      = "allow-observability-collector-to-local-kubelet"
      namespace = var.namespace
      labels    = local.collector_labels
    }
    spec = {
      endpointSelector = {
        matchLabels = { "app.kubernetes.io/name" = local.collector_name }
      }
      egress = [{
        toEntities = ["host"]
        toPorts = [{
          ports = [{
            port     = "10250"
            protocol = "TCP"
          }]
        }]
      }]
    }
  }
}

resource "kubernetes_network_policy" "compactor_egress" {
  metadata {
    name      = "allow-telemetry-data-egress"
    namespace = var.namespace
    labels    = local.compactor_labels
  }

  spec {
    pod_selector {
      match_labels = { "app.kubernetes.io/name" = local.compactor_name }
    }
    policy_types = ["Egress"]

    # DNS for the public S3 endpoint.
    egress {
      to {
        namespace_selector {
          match_labels = { "kubernetes.io/metadata.name" = "kube-system" }
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

    egress {
      to {
        ip_block {
          cidr   = "0.0.0.0/0"
          except = ["10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"]
        }
      }
      ports {
        protocol = "TCP"
        port     = "443"
      }
    }
  }
}
