output "hub_service_account_email" {
  description = "Put this into the chart's serviceAccount.annotations (iam.gke.io/gcp-service-account). Empty when hub_member is set."
  value       = var.hub_member == "" ? google_service_account.hub[0].email : ""
}

output "topics" {
  description = "The topics: the hub's and one per agent."
  value = {
    to_hub   = google_pubsub_topic.to_hub.name
    to_agent = { for name, topic in google_pubsub_topic.to_agent : name => topic.name }
  }
}

output "subscriptions" {
  description = "The subscriptions: the hub's and one per agent."
  value = {
    hub   = google_pubsub_subscription.hub.name
    agent = { for name, sub in google_pubsub_subscription.agent : name => sub.name }
  }
}

output "image_repository" {
  description = "Where the hub's image goes (the chart's image.repository), or empty when create_registry is false."
  value       = var.create_registry ? "${var.region}-docker.pkg.dev/${var.project_id}/${var.registry_id}/petrvs-bridge-hub" : ""
}

output "helm_values" {
  description = "The part of the chart's values that comes from here. Add image.digest, hub.chatId and agents.files, then use it with `helm install -f`."
  value = yamlencode({
    image = {
      repository = var.create_registry ? "${var.region}-docker.pkg.dev/${var.project_id}/${var.registry_id}/petrvs-bridge-hub" : ""
    }
    pubsub = {
      project = var.project_id
      prefix  = var.prefix
    }
    serviceAccount = {
      annotations = var.hub_member == "" ? { "iam.gke.io/gcp-service-account" = google_service_account.hub[0].email } : {}
    }
  })
}
