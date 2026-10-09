locals {
  labels = merge({ app = "petrvs-bridge" }, var.labels)

  to_hub  = "${var.prefix}-to-hub"
  hub_sub = "${var.prefix}-hub"

  # One entry per (agent, member): who may run which agent.
  agent_members = {
    for pair in flatten([
      for agent, members in var.agents : [for member in members : { agent = agent, member = member }]
    ]) : "${pair.agent}/${pair.member}" => pair
  }

  wi_project = var.workload_identity_project != "" ? var.workload_identity_project : var.project_id

  # Who the hub is in IAM: the pod's own principal when hub_member is given, else the service account created below.
  hub_member = var.hub_member != "" ? var.hub_member : "serviceAccount:${google_service_account.hub[0].email}"
}

# ---------------------------------------------------------------- the bus

resource "google_pubsub_topic" "to_hub" {
  name   = local.to_hub
  labels = local.labels
}

resource "google_pubsub_topic" "to_agent" {
  for_each = var.agents

  name   = "${var.prefix}-to-agent-${each.key}"
  labels = local.labels
}

resource "google_pubsub_subscription" "hub" {
  name   = local.hub_sub
  topic  = google_pubsub_topic.to_hub.id
  labels = local.labels

  ack_deadline_seconds       = var.ack_deadline_seconds
  message_retention_duration = var.message_retention

  # The subscription must not expire while nobody uses it (the default is 31 days).
  expiration_policy {
    ttl = ""
  }
}

resource "google_pubsub_subscription" "agent" {
  for_each = var.agents

  name   = "${var.prefix}-agent-${each.key}"
  topic  = google_pubsub_topic.to_agent[each.key].id
  labels = local.labels

  ack_deadline_seconds       = var.ack_deadline_seconds
  message_retention_duration = var.message_retention

  expiration_policy {
    ttl = ""
  }
}

# ---------------------------------------------------------------- the hub's identity (GKE workload identity)

resource "google_service_account" "hub" {
  count = var.hub_member == "" ? 1 : 0

  account_id   = var.hub_service_account_id
  display_name = "petrvs-bridge hub"
}

# The pod's Kubernetes service account may act as the Google service account. No key file exists anywhere.
resource "google_service_account_iam_member" "hub_workload_identity" {
  count = var.hub_member == "" ? 1 : 0

  service_account_id = google_service_account.hub[0].name
  role               = "roles/iam.workloadIdentityUser"
  member             = "serviceAccount:${local.wi_project}.svc.id.goog[${var.kubernetes_namespace}/${var.kubernetes_service_account}]"
}

# The hub publishes to each agent's topic and reads its own subscription. Roles sit on the resources, never on the project.
resource "google_pubsub_topic_iam_member" "hub_publishes_to_agent" {
  for_each = var.agents

  topic  = google_pubsub_topic.to_agent[each.key].name
  role   = "roles/pubsub.publisher"
  member = local.hub_member
}

resource "google_pubsub_subscription_iam_member" "hub_reads" {
  subscription = google_pubsub_subscription.hub.name
  role         = "roles/pubsub.subscriber"
  member       = local.hub_member
}

# ---------------------------------------------------------------- the agents

# An agent publishes to the hub's topic and reads its own subscription: it cannot read the hub's, nor another agent's.
resource "google_pubsub_topic_iam_member" "agent_publishes_to_hub" {
  for_each = local.agent_members

  topic  = google_pubsub_topic.to_hub.name
  role   = "roles/pubsub.publisher"
  member = each.value.member
}

resource "google_pubsub_subscription_iam_member" "agent_reads" {
  for_each = local.agent_members

  subscription = google_pubsub_subscription.agent[each.value.agent].name
  role         = "roles/pubsub.subscriber"
  member       = each.value.member
}

# ---------------------------------------------------------------- the image repository

resource "google_artifact_registry_repository" "bridge" {
  count = var.create_registry ? 1 : 0

  repository_id = var.registry_id
  location      = var.region
  format        = "DOCKER"
  description   = "Image of the petrvs-bridge hub"
  labels        = local.labels
}

resource "google_artifact_registry_repository_iam_member" "readers" {
  for_each = var.create_registry ? toset(var.registry_readers) : toset([])

  repository = google_artifact_registry_repository.bridge[0].name
  location   = var.region
  role       = "roles/artifactregistry.reader"
  member     = each.value
}
