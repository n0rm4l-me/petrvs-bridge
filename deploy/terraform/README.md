# Terraform for the hub's cloud side

Creates what the hub and the agents need in Google Cloud: the topics and subscriptions, the hub's identity, who may run
each agent, and (optionally) the image repository. It replaces steps 1 to 3 of [docs/DEPLOY.md](../../docs/DEPLOY.md).
The hub itself is installed with the Helm chart in [../helm](../helm). **Not yet applied to a real project.**

```sh
cd deploy/terraform
cp terraform.tfvars.example terraform.tfvars        # edit; the file is ignored by git
terraform init
terraform plan                                      # read all of it
terraform apply
terraform output -raw helm_values                   # the part of the chart's values that comes from here
```

What it makes, for an agent called `laptop` and the prefix `pb`:

| Resource | Access |
|---|---|
| topic `pb-to-hub` | the agent's members publish |
| subscription `pb-hub` (on `pb-to-hub`) | the hub reads |
| topic `pb-to-agent-laptop` | the hub publishes |
| subscription `pb-agent-laptop` (on that topic) | the agent's members read |

Every role sits on one topic or subscription, never on the project, and an agent can neither read the hub's subscription
nor another agent's. The subscriptions never expire and keep an unread message for an hour (a command is stale after
minutes anyway).

**The hub's identity.** Two ways, chosen by `hub_member`:

* empty (the default): a Google service account `bridge-hub` is created, the pod's Kubernetes service account may
  impersonate it (`kubernetes_namespace`, `kubernetes_service_account`), and the chart gets its address in
  `serviceAccount.annotations`;
* a principal such as `principal://iam.googleapis.com/projects/NUMBER/locations/global/workloadIdentityPools/PROJECT.svc.id.goog/subject/ns/NAMESPACE/sa/KSA`
  (GKE Workload Identity Federation): no service account is made, the roles go to the pod's own identity, and the chart
  needs no annotation.

The Kubernetes service account is called `RELEASE-petrvs-bridge-hub` by the chart (`bridge-petrvs-bridge-hub` for a release
named `bridge`) unless `serviceAccount.name` is set.

**Secrets are not here.** The bot token and the hub's private identity are never in Terraform or its state. Create them as
Kubernetes Secrets, or as secrets in your secret store that an operator such as External Secrets turns into the two
Secrets the chart reads (`secrets.botToken.secretName`, `secrets.identity.secretName`).

If your project already has an image repository, set `create_registry = false` and use its address as the chart's
`image.repository`. If the pod cannot pull the image (`ImagePullBackOff`, 403), add the nodes' service account to
`registry_readers`.

The state is local unless you add a backend (see `versions.tf`). It holds no secret but names your resources, so it is
ignored by git. Remove everything with `terraform destroy` (after `helm uninstall`).
