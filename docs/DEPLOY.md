# Deploying the hub

The hub is the only part that runs in the cloud. It needs outbound access to Telegram and to Pub/Sub and listens on nothing.
The agent is not deployed like this: it runs next to the terminal it controls, as a plain binary (`make dist`), and only
talks to Pub/Sub.

**None of the commands below has been run against a real project yet.** Read them before you use them.

## 1. The image

```sh
gcloud artifacts repositories create bridge --repository-format=docker --location=REGION
gcloud auth configure-docker REGION-docker.pkg.dev
docker buildx build --platform linux/amd64 -f deploy/hub.Dockerfile \
  -t REGION-docker.pkg.dev/PROJECT/bridge/petrvs-bridge-hub:0.1.0 --push .
```

Always give `--platform`. Without it the image is built for the machine that builds it: on an Apple silicon Mac that is
`linux/arm64`, and the pod of an amd64 node then crash-loops with `exec format error`. Use `linux/amd64` for the usual GKE
nodes and `linux/arm64` for arm nodes (Tau T2A, Axion); `--platform linux/amd64,linux/arm64` makes one image for both. The
Go stage of the Dockerfile cross-compiles, so no emulation is needed. The Dockerfile needs BuildKit (`docker buildx`
always has it). The build stage sets `GOTOOLCHAIN=auto`, because the official `golang` image sets `local` and would
refuse `go.mod` when its own Go is older than the `go` line there.

Without Docker on the machine: `gcloud builds submit --config deploy/cloudbuild.yaml --substitutions _IMAGE=... .` (this
uploads the source to Google Cloud Storage; `_PLATFORM`, default `linux/amd64`, sets the platform). Use the image's digest
(`sha256:...`) in the chart, not a moving tag.

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

Keep the ids in quotes (`"123456789"`, and `"-1001234567890"` for a group; with `--set` use `--set-string`): an unquoted
number is read by Helm as a float and would reach the hub as `1.23456789e+08`. Every key under `agents.files` must be called
`NAME.pub.json`, because the hub reads only those files. The chart's `values.schema.json` refuses both mistakes when you run
`helm lint`, `template` or `install`.

The hub logs JSON lines on standard output with the fields `severity` (`DEBUG`, `INFO`, `WARNING`, `ERROR`) and `message`,
which Cloud Logging reads on GKE, so a log-based alert on `severity>=ERROR` works. They never contain message text or tokens.

When the hub is stopped (an upgrade, a drain) it makes one last Telegram call of at most 2 s to confirm the updates it has handled, so that its successor does not type them again; Kubernetes' default 30 s grace period is ample.

Only one replica may run. The chart fixes that, because two hubs would fight over one bot.

**The lockdown does not survive a new pod.** The hub keeps `/lockdown` in a file (`PB_LOCK_FILE`), and the chart puts
that file on an `emptyDir`. An `emptyDir` survives a restart of the container but not a replacement of the pod, and a
`helm upgrade` that changes the pod (a new image, a changed `agents.files`), a node drain and an eviction all replace
it. With the `Recreate` strategy the new hub then always starts unlocked, and says nothing in the chat; an agent that
was offline when the lockdown was ordered never heard it, and the hub relays to it again. If a lockdown was on, send
`/lockdown` again after such an event (it is safe to repeat). To keep it across pods, back the `state` volume with a
small PersistentVolumeClaim (a few KiB, `ReadWriteOnce`) instead of the `emptyDir` in `templates/deployment.yaml`;
`Recreate` already makes sure two pods never hold the volume at once. The chart has no value for this yet. Run as a
plain binary without `PB_LOCK_FILE`, the hub forgets a lockdown on every restart.

## 6. The agent

On the laptop the agent needs Google credentials with the publisher role on `pb-to-hub` and the subscriber role on
`pb-agent-laptop`: Application Default Credentials (`gcloud auth application-default login`), a short-lived credential from
workload identity federation, or a key file in `GOOGLE_APPLICATION_CREDENTIALS` (mode 0600). Prefer the first two.
