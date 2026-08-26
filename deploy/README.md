# Deploying discord-dnd-bot with ArgoCD

`argocd-application.yaml` deploys the chart with every feature this environment
uses enabled: bundled PostgreSQL (pgvector) + Redis, self-hosted STT, an
ACK-provisioned S3 bucket + IAM role (IRSA), a Prometheus `ServiceMonitor`, the
Grafana dashboard, and secrets pulled from AWS Secrets Manager via the External
Secrets Operator.

## 1. Fill in the placeholders

Edit `argocd-application.yaml` and replace:

| Placeholder | What to put |
| --- | --- |
| `<TARGET_REVISION>` | git branch or tag to track (e.g. `main`) |
| `<AWS_ACCOUNT_ID>` | 12-digit AWS account ID (for the IRSA role trust) |
| `<EKS_OIDC_PROVIDER>` | Cluster OIDC issuer, no scheme, e.g. `oidc.eks.us-east-1.amazonaws.com/id/EXAMPLE` |
| `<CLUSTER_SECRET_STORE>` | Your External Secrets `ClusterSecretStore` that reads Secrets Manager |
| `<DISCORD_APP_ID>` | Discord application (client) ID |
| `<DISCORD_GUILD_ID>` | Your Discord server ID(s) |

## 2. Create the secret in AWS Secrets Manager

Create **one** JSON secret named **`discord-dnd-bot/production`** with these
properties (the ExternalSecret maps each to a Kubernetes Secret key):

| Secrets Manager property | Kubernetes key | Required? | Notes |
| --- | --- | --- | --- |
| `discord_token` | `DISCORD_TOKEN` | **Yes** | Discord bot token |
| `database_password` | `DATABASE_PASSWORD` | **Yes** | Password for the DB user. The bot builds the connection string from this + the non-secret parts in `config.database`. The bundled PostgreSQL uses this same value, so the password lives in exactly one place. |
| `litellm_api_key` | `LITELLM_API_KEY` | **Yes** | API key for your LiteLLM proxy |

Example (AWS CLI):

```bash
aws secretsmanager create-secret \
  --name discord-dnd-bot/production \
  --secret-string '{
    "discord_token": "REPLACE",
    "database_password": "REPLACE-strong-password",
    "litellm_api_key": "REPLACE"
  }'
```

> The bundled PostgreSQL reads `DATABASE_PASSWORD` from the same synced Secret,
> so there is no second place to keep the DB password in sync.

## 3. Register the STT route in LiteLLM

The model inventory has no speech-to-text model, so this deployment runs the
self-hosted Whisper service (`stt.enabled=true`). Add a matching route to your
LiteLLM `config.yaml` so `/session` transcription works:

```yaml
model_list:
  - model_name: voice-transcribe
    litellm_params:
      model: openai/Systran/faster-whisper-small
      api_base: http://discord-dnd-bot-stt.discord-dnd-bot:8000/v1
      api_key: "none"
    model_info: { mode: audio_transcription }
```

Everything except transcription (and the notes/`ask` that depend on it) works
without this route.

## 4. Apply

```bash
kubectl apply -f deploy/argocd-application.yaml
```

## Model mapping

| Bot task | LiteLLM model |
| --- | --- |
| chat / `/lore` / `/ask` | `claude-4-5-sonnet` |
| `/session` notes | `claude-opus-4-5` |
| `/recap` | `claude-4-5-haiku` |
| embeddings (`/ask` retrieval) | `amazon-titan-embed-text-v2:0` (dim **1024**) |
| `/art` | `gpt-image-2` |
| transcription | `voice-transcribe` (self-hosted Whisper, see step 3) |

## Prerequisites in the cluster

- **ArgoCD** (Application is created in the `argocd` namespace).
- **External Secrets Operator** with a `ClusterSecretStore` for AWS Secrets Manager.
- **ACK controllers** for S3 and IAM (`s3.services.k8s.aws`, `iam.services.k8s.aws`).
- **Prometheus Operator** (for the `ServiceMonitor`) and **Grafana** with the
  dashboard sidecar (for `grafanaDashboard`).
- A default **StorageClass** (bundled PostgreSQL/Redis/STT request PVCs).
