# Deploying the hub

The hub is the only part that runs in the cloud. It needs outbound access to Telegram and to Pub/Sub and listens on nothing.
The agent is not deployed like this: it runs next to the terminal it controls, as a plain binary (`make dist`), and only
talks to Pub/Sub.

**None of the commands below has been run against a real project yet.** Read them before you use them.

## 1. The image

```sh
gcloud artifacts repositories create bridge --repository-format=docker --location=REGION
gcloud auth configure-docker REGION-docker.pkg.dev
docker build -f deploy/hub.Dockerfile -t REGION-docker.pkg.dev/PROJECT/bridge/petrvs-bridge-hub:0.1.0 .
docker push REGION-docker.pkg.dev/PROJECT/bridge/petrvs-bridge-hub:0.1.0
```

Without Docker on the machine: `gcloud builds submit --config deploy/cloudbuild.yaml --substitutions _IMAGE=... .` (this
uploads the source to Google Cloud Storage). Use the image's digest (`sha256:...`) in the chart, not a moving tag.

## 2. Pub/Sub

For prefix `pb` and an agent called `laptop` (see the README for the properties and for the per-party access rules):

```sh
gcloud pubsub topics create pb-to-hub pb-to-agent-laptop
gcloud pubsub subscriptions create pb-hub --topic=pb-to-hub --ack-deadline=30 --message-retention-duration=1h --expiration-period=never
gcloud pubsub subscriptions create pb-agent-laptop --topic=pb-to-agent-laptop --ack-deadline=30 --message-retention-duration=1h --expiration-period=never
```

## 3. The hub's identity in Google Cloud (GKE Workload Identity)

```sh
gcloud iam service-accounts create bridge-hub
# the Kubernetes service account is NAMESPACE/RELEASE-petrvs-bridge-hub unless you set serviceAccount.name
gcloud iam service-accounts add-iam-policy-binding bridge-hub@PROJECT.iam.gserviceaccount.com \
  --role=roles/iam.workloadIdentityUser --member="serviceAccount:PROJECT.svc.id.goog[NAMESPACE/KSA_NAME]"
gcloud pubsub topics add-iam-policy-binding pb-to-agent-laptop --member=serviceAccount:bridge-hub@PROJECT.iam.gserviceaccount.com --role=roles/pubsub.publisher
gcloud pubsub subscriptions add-iam-policy-binding pb-hub --member=serviceAccount:bridge-hub@PROJECT.iam.gserviceaccount.com --role=roles/pubsub.subscriber
```

Put the Google service account in the chart's `serviceAccount.annotations` (`iam.gke.io/gcp-service-account`).

## 4. Secrets and keys

```sh
kubectl create namespace bridge
kubectl -n bridge create secret generic petrvs-bridge-bot --from-file=token=/path/to/token-file
kubectl -n bridge create secret generic petrvs-bridge-hub-identity --from-file=hub.identity.json=keys/hub.identity.json
```

The agents' public bundles (`NAME.pub.json`) go into the chart's `agents.files`; they are not secret.

## 5. The chart

```sh
helm lint deploy/helm/petrvs-bridge-hub -f my-values.yaml
helm install bridge deploy/helm/petrvs-bridge-hub -n bridge -f my-values.yaml
# or publish it as an OCI artefact in Artifact Registry
helm package deploy/helm/petrvs-bridge-hub
helm push petrvs-bridge-hub-0.1.0.tgz oci://REGION-docker.pkg.dev/PROJECT/bridge
```

A minimal `my-values.yaml`:

```yaml
image: {repository: REGION-docker.pkg.dev/PROJECT/bridge/petrvs-bridge-hub, digest: "sha256:..."}
pubsub: {project: PROJECT}
hub: {chatId: "123456789"}
serviceAccount:
  annotations: {iam.gke.io/gcp-service-account: bridge-hub@PROJECT.iam.gserviceaccount.com}
agents:
  files:
    laptop.pub.json: |
      { ...the contents of keys/laptop.pub.json... }
```

Only one replica may run. The chart fixes that, because two hubs would fight over one bot.

## 6. The agent

On the laptop the agent needs Google credentials with the publisher role on `pb-to-hub` and the subscriber role on
`pb-agent-laptop`: Application Default Credentials (`gcloud auth application-default login`), a short-lived credential from
workload identity federation, or a key file in `GOOGLE_APPLICATION_CREDENTIALS` (mode 0600). Prefer the first two.
