# =============================================================================
# tunnel.tf — Cloudflare Tunnel: the only ingress path to the cluster
# =============================================================================
#
# A single remotely-managed Cloudflare Tunnel terminates all external traffic at
# the Cloudflare edge and forwards it, over an OUTBOUND-only connection from
# inside the cluster, to ingress-nginx. ingress-nginx then host-routes to tenant
# Services exactly as before. There is no public origin: see
# task-for-infra-cloudflare-to-infra-tunnel.md.
#
#   internet ──TLS──> Cloudflare edge ──(outbound tunnel)──> cloudflared (infra)
#                          └─> http://ingress-nginx-controller.infra.svc:80 ─> tenant svc
#
# DNS cutover (proxied CNAME <host> -> <tunnel-id>.cfargotunnel.com) is done
# per-host in cloudflare.tf as apps deploy — NOT here.
#
# Unclaimed hostnames are served by the "nothing deployed" default backend
# (default-backend.tf), reached because this tunnel routes every host to
# ingress-nginx and nginx falls through when no Ingress claims the host.
# It answers HTTP 503 with a styled page — not a bare 404. Diagnostics that
# expect a 404 for an unclaimed host are wrong.
#
resource "random_id" "tunnel_secret" {
  byte_length = 32
}

# The tunnel itself. config_src = "cloudflare" => ingress config is managed via
# the API (the *_config resource below), not a local config file.
resource "cloudflare_zero_trust_tunnel_cloudflared" "infra" {
  account_id    = var.cloudflare_account_id
  name          = var.tunnel_name
  config_src    = "cloudflare"
  tunnel_secret = random_id.tunnel_secret.b64_std
}

# Single catch-all ingress rule: everything -> ingress-nginx, which host-routes.
resource "cloudflare_zero_trust_tunnel_cloudflared_config" "infra" {
  account_id = var.cloudflare_account_id
  tunnel_id  = cloudflare_zero_trust_tunnel_cloudflared.infra.id

  config = {
    ingress = [
      {
        service = "http://ingress-nginx-controller.${var.namespace}.svc.cluster.local:80"
      }
    ]
  }
}

# Connector run token (sensitive) for `cloudflared tunnel run`.
data "cloudflare_zero_trust_tunnel_cloudflared_token" "infra" {
  account_id = var.cloudflare_account_id
  tunnel_id  = cloudflare_zero_trust_tunnel_cloudflared.infra.id
}

# -----------------------------------------------------------------------------
# In-cluster connector: cloudflared Deployment (infra namespace)
# -----------------------------------------------------------------------------

locals {
  cloudflared_labels = {
    "app.kubernetes.io/name"       = "cloudflared"
    "app.kubernetes.io/component"  = "tunnel"
    "app.kubernetes.io/part-of"    = "platform-infra"
    "app.kubernetes.io/managed-by" = "opentofu"
  }
  # Pinned by digest (manifest list; containerd resolves node arch).
  cloudflared_image = "cloudflare/cloudflared:2026.6.1@sha256:6d91c121b803126f7a5344005d17a9324788fc09d305b6e2560ec6040a7ae283"
}

resource "kubernetes_secret" "cloudflared_token" {
  metadata {
    name      = "cloudflared-tunnel-token"
    namespace = var.namespace
    labels    = local.cloudflared_labels
  }

  data = {
    TUNNEL_TOKEN = data.cloudflare_zero_trust_tunnel_cloudflared_token.infra.token
  }

  type = "Opaque"
}

resource "kubernetes_deployment_v1" "cloudflared" {
  metadata {
    name      = "cloudflared"
    namespace = var.namespace
    labels    = local.cloudflared_labels
  }

  # Ensure the egress policy exists before pods start, so the first connection
  # attempt to the Cloudflare edge (TCP/7844) is already permitted.
  depends_on = [
    kubernetes_network_policy.infra_cloudflared_egress,
  ]

  spec {
    replicas = 2

    selector {
      match_labels = {
        "app.kubernetes.io/name" = "cloudflared"
      }
    }

    template {
      metadata {
        labels = local.cloudflared_labels
      }

      spec {
        automount_service_account_token = false
        node_selector                   = var.node_selector

        security_context {
          run_as_non_root = true
          run_as_user     = 65532
          seccomp_profile {
            type = "RuntimeDefault"
          }
        }

        container {
          name              = "cloudflared"
          image             = local.cloudflared_image
          image_pull_policy = "IfNotPresent"

          # TUNNEL_TOKEN (env) is consumed by `tunnel run`. http2 transport keeps
          # all edge traffic on TCP/443 (no QUIC/7844), reusing existing egress.
          args = ["tunnel", "--no-autoupdate", "--metrics", "0.0.0.0:2000", "run"]

          env {
            name = "TUNNEL_TOKEN"
            value_from {
              secret_key_ref {
                name = kubernetes_secret.cloudflared_token.metadata[0].name
                key  = "TUNNEL_TOKEN"
              }
            }
          }

          env {
            name  = "TUNNEL_TRANSPORT_PROTOCOL"
            value = "http2"
          }

          port {
            name           = "metrics"
            container_port = 2000
          }

          resources {
            requests = {
              cpu    = "50m"
              memory = "64Mi"
            }
            limits = {
              cpu    = "200m"
              memory = "128Mi"
            }
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
              path = "/ready"
              port = 2000
            }
            initial_delay_seconds = 30
            period_seconds        = 30
            failure_threshold     = 3
          }

          readiness_probe {
            http_get {
              path = "/ready"
              port = 2000
            }
            initial_delay_seconds = 10
            period_seconds        = 10
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

        # Spread the two replicas across nodes for HA.
        affinity {
          pod_anti_affinity {
            required_during_scheduling_ignored_during_execution {
              topology_key = "kubernetes.io/hostname"
              label_selector {
                match_labels = {
                  "app.kubernetes.io/name" = "cloudflared"
                }
              }
            }
          }
        }
      }
    }
  }
}

resource "kubernetes_pod_disruption_budget_v1" "cloudflared" {
  metadata {
    name      = "cloudflared"
    namespace = var.namespace
    labels    = local.cloudflared_labels
  }

  spec {
    min_available = 1
    selector {
      match_labels = {
        "app.kubernetes.io/name" = "cloudflared"
      }
    }
  }
}

# The only ingress cloudflared accepts: node-local observability collectors
# scraping its fixed metrics port. Tunnel traffic itself is outbound-only.
resource "kubernetes_network_policy" "infra_cloudflared_metrics_ingress" {
  metadata {
    name      = "allow-collector-to-cloudflared-metrics"
    namespace = var.namespace
  }

  spec {
    pod_selector {
      match_labels = {
        "app.kubernetes.io/name" = "cloudflared"
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
        port     = "2000"
      }
    }
  }
}

# Egress for cloudflared: outbound 443 to the Cloudflare edge (http2 transport),
# and TCP/80 to the in-cluster ingress-nginx controller. DNS is already allowed
# for all infra pods by the allow-dns policy. Its only ingress is the collector
# metrics scrape above (kubelet health probes bypass NetworkPolicy).
resource "kubernetes_network_policy" "infra_cloudflared_egress" {
  metadata {
    name      = "allow-cloudflared-egress"
    namespace = var.namespace
  }

  spec {
    pod_selector {
      match_labels = {
        "app.kubernetes.io/name" = "cloudflared"
      }
    }

    policy_types = ["Egress"]

    # Cloudflare API (connector registration / token refresh) on 443.
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

    # Cloudflare Tunnel edge on TCP/7844 (http2 transport — NOT 443). Restricted
    # to Cloudflare's published cloudflared edge ranges (region1/region2). If
    # tunnels ever fail to connect after a Cloudflare edge-IP change, widen these.
    egress {
      to {
        ip_block {
          cidr = "198.41.192.0/24"
        }
      }
      to {
        ip_block {
          cidr = "198.41.200.0/24"
        }
      }
      ports {
        protocol = "TCP"
        port     = "7844"
      }
    }

    # Forward to the ingress-nginx controller in-cluster.
    egress {
      to {
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
      ports {
        protocol = "TCP"
        port     = "80"
      }
    }
  }
}

# =============================================================================
# default-backend.tf — "nothing deployed" page for unmatched hosts
# =============================================================================
#
# ingress-nginx forwards any request whose host/path matches no tenant Ingress
# to this Service (wired via controller --default-backend-service). It serves a
# small static page with HTTP 503, so a beta.* host that points at the tunnel
# but has no app deployed yet renders "nothing deployed" instead of a bare 404.
#
# Lives in-cluster (behind the tunnel). The alternative — serving this at the
# Cloudflare edge (Worker / custom error asset) — stays open; this is the simple
# uniform default for every host routed through the tunnel.
# =============================================================================

locals {
  nothing_deployed_labels = {
    "app.kubernetes.io/name"       = "nothing-deployed"
    "app.kubernetes.io/component"  = "default-backend"
    "app.kubernetes.io/part-of"    = "platform-infra"
    "app.kubernetes.io/managed-by" = "opentofu"
  }
  # Unprivileged nginx (runs as uid 101 on :8080), pinned by digest.
  nothing_deployed_image = "nginxinc/nginx-unprivileged:1.30-alpine@sha256:fafa1102c789119971b3d83f9293f1ef5526bc73583a12e13ff5cd1299ed8b6c"
}

resource "kubernetes_config_map" "nothing_deployed" {
  metadata {
    name      = "nothing-deployed"
    namespace = var.namespace
    labels    = local.nothing_deployed_labels
  }

  data = {
    "default.conf" = <<-EOT
      server {
          listen 8080 default_server;
          server_name _;
          root /usr/share/nginx/html;

          location = /healthz {
              access_log off;
              add_header Content-Type text/plain;
              return 200 "ok\n";
          }

          # Serve the page (with the 503 status preserved) for everything else.
          location = /index.html {
              internal;
          }
          location / {
              return 503;
          }
          error_page 503 /index.html;
      }
    EOT

    "index.html" = <<-EOT
      <!doctype html>
      <html lang="en">
      <head>
        <meta charset="utf-8">
        <meta name="viewport" content="width=device-width, initial-scale=1">
        <title>Not deployed yet</title>
        <style>
          :root { color-scheme: light dark; }
          html, body { height: 100%; margin: 0; }
          body {
            display: grid; place-items: center;
            font: 16px/1.5 system-ui, -apple-system, Segoe UI, Roboto, sans-serif;
            background: Canvas; color: CanvasText;
          }
          main { text-align: center; padding: 2rem; max-width: 32rem; }
          h1 { font-size: 1.5rem; margin: 0 0 .5rem; }
          p { margin: .25rem 0; opacity: .75; }
          .dot { display:inline-block; width:.5rem; height:.5rem; border-radius:50%;
                 background:#888; margin-right:.5rem; vertical-align:middle; }
        </style>
      </head>
      <body>
        <main>
          <h1><span class="dot"></span>Nothing deployed here yet</h1>
          <p>This hostname is reachable but no service is running at it.</p>
          <p>If you expected an app, it hasn't been deployed to the cluster yet.</p>
        </main>
      </body>
      </html>
    EOT
  }
}

resource "kubernetes_deployment_v1" "nothing_deployed" {
  metadata {
    name      = "nothing-deployed"
    namespace = var.namespace
    labels    = local.nothing_deployed_labels
  }



  spec {
    # The fallback page is deliberately non-critical; one replica is enough.
    replicas = 1

    selector {
      match_labels = {
        "app.kubernetes.io/name" = "nothing-deployed"
      }
    }

    template {
      metadata {
        labels = local.nothing_deployed_labels
        annotations = {
          # Roll pods when the page/config changes.
          "checksum/config" = sha1(jsonencode(kubernetes_config_map.nothing_deployed.data))
        }
      }

      spec {
        automount_service_account_token = false
        node_selector                   = var.node_selector

        security_context {
          run_as_non_root = true
          run_as_user     = 101
          seccomp_profile {
            type = "RuntimeDefault"
          }
        }

        container {
          name              = "nginx"
          image             = local.nothing_deployed_image
          image_pull_policy = "IfNotPresent"

          port {
            name           = "http"
            container_port = 8080
          }

          resources {
            requests = {
              cpu    = "10m"
              memory = "16Mi"
            }
            limits = {
              cpu    = "100m"
              memory = "64Mi"
            }
          }

          security_context {
            allow_privilege_escalation = false
            run_as_non_root            = true
            run_as_user                = 101
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
          }

          readiness_probe {
            http_get {
              path = "/healthz"
              port = 8080
            }
            initial_delay_seconds = 3
            period_seconds        = 10
          }

          volume_mount {
            name       = "config"
            mount_path = "/etc/nginx/conf.d/default.conf"
            sub_path   = "default.conf"
            read_only  = true
          }

          volume_mount {
            name       = "config"
            mount_path = "/usr/share/nginx/html/index.html"
            sub_path   = "index.html"
            read_only  = true
          }
        }

        volume {
          name = "config"
          config_map {
            name = kubernetes_config_map.nothing_deployed.metadata[0].name
          }
        }

        affinity {
          pod_anti_affinity {
            required_during_scheduling_ignored_during_execution {
              topology_key = "kubernetes.io/hostname"
              label_selector {
                match_labels = {
                  "app.kubernetes.io/name" = "nothing-deployed"
                }
              }
            }
          }
        }
      }
    }
  }
}

resource "kubernetes_service_v1" "nothing_deployed" {
  metadata {
    name      = "nothing-deployed"
    namespace = var.namespace
    labels    = local.nothing_deployed_labels
  }

  spec {
    selector = {
      "app.kubernetes.io/name" = "nothing-deployed"
    }
    port {
      name        = "http"
      port        = 80
      target_port = 8080
      protocol    = "TCP"
    }
    type = "ClusterIP"
  }
}

# Allow the ingress-nginx controller to reach the default backend on 8080.
resource "kubernetes_network_policy" "infra_nothing_deployed_ingress" {
  metadata {
    name      = "allow-nothing-deployed-ingress"
    namespace = var.namespace
  }

  spec {
    pod_selector {
      match_labels = {
        "app.kubernetes.io/name" = "nothing-deployed"
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
        port     = "8080"
      }
    }
  }
}
