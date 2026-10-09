variable "project_id" {
  description = "The Google Cloud project that holds the topics, the subscriptions and the hub's service account."
  type        = string
}

variable "region" {
  description = "Region of the image repository (and the default region of the provider)."
  type        = string
}

variable "prefix" {
  description = "Prefix of the Pub/Sub resources: PREFIX-to-hub, PREFIX-hub, PREFIX-to-agent-NAME, PREFIX-agent-NAME. Must equal PB_PREFIX of the hub and of every agent (the chart's pubsub.prefix)."
  type        = string
  default     = "pb"

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{0,15}$", var.prefix))
    error_message = "The prefix is 1 to 16 characters: lower-case letters, digits and dashes, starting with a letter."
  }
}

variable "agents" {
  description = <<-EOT
    The agents: a map from the agent's name to the IAM members that run it (for example "user:me@example.com",
    "group:team@example.com" or "serviceAccount:agent@PROJECT.iam.gserviceaccount.com"). Each member may publish to the
    hub's topic and read its own agent's subscription, and nothing else. A name follows the agent's own rule: lower-case
    letters, digits, dash and underscore, at most 32 characters, starting with a letter or a digit.
  EOT
  type        = map(list(string))

  validation {
    condition     = alltrue([for name in keys(var.agents) : can(regex("^[a-z0-9][a-z0-9_-]{0,31}$", name))])
    error_message = "An agent name is 1 to 32 characters: lower-case letters, digits, dash and underscore, starting with a letter or a digit."
  }

  validation {
    condition = alltrue(flatten([
      for members in values(var.agents) : [for m in members : can(regex("^(user|group|serviceAccount|principal|principalSet):.+", m))]
    ]))
    error_message = "Every member is written as user:..., group:..., serviceAccount:..., principal:... or principalSet:...; allUsers and allAuthenticatedUsers are refused."
  }
}

variable "hub_member" {
  description = <<-EOT
    The IAM member the hub runs as, when it needs no Google service account: with GKE Workload Identity Federation the
    pod's Kubernetes service account is itself a principal, for example
    "principal://iam.googleapis.com/projects/PROJECT_NUMBER/locations/global/workloadIdentityPools/PROJECT_ID.svc.id.goog/subject/ns/NAMESPACE/sa/KSA".
    Empty: a Google service account is created and the pod impersonates it (the chart's serviceAccount.annotations).
  EOT
  type        = string
  default     = ""

  validation {
    condition     = var.hub_member == "" || can(regex("^(principal|principalSet|serviceAccount):.+", var.hub_member))
    error_message = "hub_member is written as principal://..., principalSet://... or serviceAccount:...; leave it empty to create a service account."
  }
}

variable "hub_service_account_id" {
  description = "Name (the part before the @) of the Google service account that the hub runs as. Unused when hub_member is set."
  type        = string
  default     = "bridge-hub"

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{4,28}[a-z0-9]$", var.hub_service_account_id))
    error_message = "A service account id is 6 to 30 characters: lower-case letters, digits and dashes, starting with a letter."
  }
}

variable "kubernetes_namespace" {
  description = "The namespace the chart is installed into."
  type        = string
  default     = "petrvs-bridge"
}

variable "kubernetes_service_account" {
  description = "The Kubernetes service account the hub pod runs as. The chart names it RELEASE-petrvs-bridge-hub unless serviceAccount.name is set (release \"bridge\" gives the default here)."
  type        = string
  default     = "bridge-petrvs-bridge-hub"
}

variable "workload_identity_project" {
  description = "The project that owns the cluster's workload identity pool (PROJECT.svc.id.goog). Empty: the same project as project_id."
  type        = string
  default     = ""
}

variable "create_registry" {
  description = "Create the Artifact Registry repository for the hub's image."
  type        = bool
  default     = true
}

variable "registry_id" {
  description = "Name of the Artifact Registry repository."
  type        = string
  default     = "bridge"
}

variable "registry_readers" {
  description = "IAM members that may pull the image from the repository, normally the service account of the cluster's nodes. Needed only if the pod cannot pull (ImagePullBackOff with a 403)."
  type        = list(string)
  default     = []
}

variable "ack_deadline_seconds" {
  description = "Acknowledgement deadline of the subscriptions. The programs extend it themselves while they work; keep it at 30."
  type        = number
  default     = 30
}

variable "message_retention" {
  description = "How long the bus keeps a message nobody has taken. A command is stale after minutes anyway."
  type        = string
  default     = "3600s"
}

variable "labels" {
  description = "Labels put on every resource that takes them."
  type        = map(string)
  default     = {}
}
