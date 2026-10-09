# Google Cloud side of the hub: Pub/Sub, the hub's service account, access for the agents, and (optionally) the image
# repository. The hub itself is deployed with the Helm chart in ../helm (see ../../docs/DEPLOY.md).
#
# NOT YET APPLIED TO A REAL PROJECT. Read `terraform plan` before you apply.

terraform {
  required_version = ">= 1.5"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = ">= 6.0"
    }
  }

  # The state stays on this machine unless you configure a backend. It holds no secret (this configuration creates no key),
  # but it names your resources: keep it out of version control (.gitignore does). For a shared state, use a bucket:
  #
  # backend "gcs" {
  #   bucket = "YOUR-STATE-BUCKET"
  #   prefix = "petrvs-bridge"
  # }
}

provider "google" {
  project = var.project_id
  region  = var.region
}
