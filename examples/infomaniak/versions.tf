terraform {
  # OpenTofu >= 1.9.0 required for client-side state encryption support.
  # OpenTofu version numbering is independent of Terraform; 1.9.x is the
  # stable release series with AES-256-GCM encryption via PBKDF2 key derivation.
  required_version = ">= 1.9.0"

  # ---------------------------------------------------------------------------
  # Client-side state encryption (OpenTofu-specific feature).
  #
  # Encrypts state and plan files with AES-256-GCM before writing to S3.
  # Key is derived from a passphrase via PBKDF2 — no KMS dependency.
  # ---------------------------------------------------------------------------
  encryption {
    key_provider "pbkdf2" "state" {
      passphrase = var.state_encryption_passphrase
    }

    method "aes_gcm" "default" {
      keys = key_provider.pbkdf2.state
    }

    state {
      method   = method.aes_gcm.default
      enforced = true
    }

    plan {
      method   = method.aes_gcm.default
      enforced = true
    }
  }

  # ---------------------------------------------------------------------------
  # Remote state: Infomaniak S3-compatible object storage (OpenStack Swift).
  #
  # OpenTofu 1.9+ uses `endpoints` block and `use_path_style` (not
  # `force_path_style`). The `skip_*` flags are required because Infomaniak's
  # S3 implementation does not expose the full AWS metadata API surface.
  #
  # Auth: set AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY environment variables
  # using the credentials from Infomaniak Public Cloud > Object Storage >
  # Credentials.
  #
  # State locking: NOT supported (Swift S3 has no DynamoDB-compatible backend).
  # Accepted mitigations:
  #   1. GitHub Actions concurrency groups (terraform-apply.yml uses
  #      `concurrency: group: terraform-apply` with cancel-in-progress: false)
  #      to guarantee only one CI apply runs at a time.
  #   2. Local applies MUST use scripts/tf-apply.sh, which queries the GitHub
  #      Actions API and refuses to proceed if a CI apply is in progress.
  # Residual risk: two developers running local applies simultaneously. Team
  # process: announce in Slack before any manual apply.
  # ---------------------------------------------------------------------------
  required_providers {
    infomaniak = {
      source  = "Infomaniak/infomaniak"
      version = "~> 1.4"
    }
    openstack = {
      source  = "terraform-provider-openstack/openstack"
      version = "~> 3.0"
    }
    cloudflare = {
      source  = "cloudflare/cloudflare"
      version = "~> 5.0"
    }
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 2.35"
    }
    helm = {
      source  = "hashicorp/helm"
      version = "~> 2.17"
    }
    postgresql = {
      source  = "cyrilgdn/postgresql"
      version = "~> 1.25"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
    tls = {
      source  = "hashicorp/tls"
      version = "~> 4.1"
    }
    local = {
      source  = "hashicorp/local"
      version = "~> 2.5"
    }
    null = {
      source  = "hashicorp/null"
      version = "~> 3.2"
    }
  }
}
