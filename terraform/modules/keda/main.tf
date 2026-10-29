# =============================================================================
# KEDA — application execution-capacity autoscaling
# =============================================================================
resource "kubernetes_namespace" "keda" {
  metadata {
    name = "keda"
    labels = {
      "kubernetes.io/metadata.name"        = "keda"
      "platform.infra/role"                = "cluster-addon"
      "pod-security.kubernetes.io/enforce" = "restricted"
      "pod-security.kubernetes.io/warn"    = "restricted"
      "pod-security.kubernetes.io/audit"   = "restricted"
    }
  }


}

resource "kubernetes_default_service_account" "keda" {
  metadata {
    namespace = kubernetes_namespace.keda.metadata[0].name
  }

  automount_service_account_token = false
}

resource "kubernetes_resource_quota" "keda" {
  metadata {
    name      = "keda-quota"
    namespace = kubernetes_namespace.keda.metadata[0].name
  }

  spec {
    hard = {
      "limits.cpu"      = "1500m"
      "limits.memory"   = "2Gi"
      "pods"            = "8"
      "requests.cpu"    = "500m"
      "requests.memory" = "512Mi"
      "services"        = "5"
    }
  }
}

resource "kubernetes_network_policy" "keda_default_deny" {
  metadata {
    name      = "default-deny"
    namespace = kubernetes_namespace.keda.metadata[0].name
  }

  spec {
    pod_selector {}
    policy_types = ["Ingress", "Egress"]
  }
}

resource "kubernetes_network_policy" "keda_allow_dns" {
  metadata {
    name      = "allow-dns"
    namespace = kubernetes_namespace.keda.metadata[0].name
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

# The managed Kubernetes Service is DNATed to this control-plane endpoint.
# Keep this synchronized with infra_allow_apiserver_egress in main.tf.
resource "kubernetes_network_policy" "keda_allow_apiserver_egress" {
  metadata {
    name      = "allow-apiserver-egress"
    namespace = kubernetes_namespace.keda.metadata[0].name
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

resource "kubernetes_network_policy" "keda_operator_metrics_ingress" {
  metadata {
    name      = "allow-operator-ingress-from-metrics-apiserver"
    namespace = kubernetes_namespace.keda.metadata[0].name
  }

  spec {
    pod_selector {
      match_labels = {
        "app" = "keda-operator"
      }
    }
    policy_types = ["Ingress"]

    ingress {
      from {
        pod_selector {
          match_labels = {
            "app" = "keda-operator-metrics-apiserver"
          }
        }
      }
      ports {
        protocol = "TCP"
        port     = "9666"
      }
    }
  }
}

resource "kubernetes_network_policy" "keda_metrics_operator_egress" {
  metadata {
    name      = "allow-metrics-apiserver-egress-to-operator"
    namespace = kubernetes_namespace.keda.metadata[0].name
  }

  spec {
    pod_selector {
      match_labels = {
        "app" = "keda-operator-metrics-apiserver"
      }
    }
    policy_types = ["Egress"]

    egress {
      to {
        pod_selector {
          match_labels = {
            "app" = "keda-operator"
          }
        }
      }
      ports {
        protocol = "TCP"
        port     = "9666"
      }
    }
  }
}

resource "kubernetes_network_policy" "keda_metrics_apiserver_ingress" {
  metadata {
    name      = "allow-external-metrics-apiserver-ingress"
    namespace = kubernetes_namespace.keda.metadata[0].name
  }

  spec {
    pod_selector {
      match_labels = {
        "app" = "keda-operator-metrics-apiserver"
      }
    }
    policy_types = ["Ingress"]

    # The managed control plane is outside the pod network, so source selectors
    # cannot identify it. Restriction is the metrics API container port.
    ingress {
      ports {
        protocol = "TCP"
        port     = "6443"
      }
    }
  }
}

resource "kubernetes_network_policy" "keda_webhook_ingress" {
  metadata {
    name      = "allow-admission-webhook-ingress"
    namespace = kubernetes_namespace.keda.metadata[0].name
  }

  spec {
    pod_selector {
      match_labels = {
        "app" = "keda-admission-webhooks"
      }
    }
    policy_types = ["Ingress"]

    ingress {
      ports {
        protocol = "TCP"
        port     = "9443"
      }
    }
  }
}

resource "helm_release" "keda" {
  name       = "keda"
  repository = "https://kedacore.github.io/charts"
  chart      = "keda"
  version    = "2.20.2"
  namespace  = kubernetes_namespace.keda.metadata[0].name
  timeout    = 600
  wait       = true
  atomic     = true

  values = [var.helm_values]

  depends_on = [
    kubernetes_resource_quota.keda,
    kubernetes_network_policy.keda_allow_apiserver_egress,
    kubernetes_network_policy.keda_allow_dns,
    kubernetes_network_policy.keda_metrics_apiserver_ingress,
    kubernetes_network_policy.keda_metrics_operator_egress,
    kubernetes_network_policy.keda_operator_metrics_ingress,
    kubernetes_network_policy.keda_webhook_ingress,
  ]
}
