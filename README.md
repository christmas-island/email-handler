# email-handler

Inbound email handler for `*@only-claws.net` — processes incoming emails, extracts OTP codes, persists to CockroachDB, and notifies Discord.

## Architecture

### Primary Path (Stalwart)

```
Stalwart Mail Server (mail.only-claws.net)
  → webhook event (message-ingest.received)
  → POST /email/stalwart-webhook
  → email-handler fetches full email via JMAP
  → extract OTPs, store in CRDB, notify Discord
```

### Legacy Path (Cloudflare — fallback)

```
*@only-claws.net → Cloudflare Email Routing → Email Worker → POST /email/inbound → email-handler
  ↓ (fallback)
  forward to Gmail
```

## Endpoints

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/` | Service status page |
| `GET` | `/health` | Liveness/readiness probe |
| `POST` | `/email/stalwart-webhook` | Stalwart webhook receiver (primary) |
| `POST` | `/email/inbound` | CF Worker inbound (legacy fallback) |
| `GET` | `/email/query/{clawname}` | Query inbox by local part |

## Features

- **OTP extraction** — regex-based extraction of verification codes and magic links
- **CockroachDB persistence** — stores all inbound emails and extracted codes
- **Discord notifications** — posts to Discord webhook, tags claws by name
- **JMAP integration** — fetches full email content from Stalwart for body analysis
- **Dual ingest** — accepts emails from both Stalwart webhooks and CF Workers

## Environment Variables

### Required

| Var | Description |
|-----|-------------|
| `HANDLER_SECRET` | Shared secret for CF Worker → API auth |

### Stalwart Integration

| Var | Description | Default |
|-----|-------------|---------|
| `STALWART_API_TOKEN` | Bearer token for Stalwart JMAP API | — |
| `STALWART_JMAP_URL` | Stalwart JMAP endpoint | `https://mail.only-claws.net/jmap` |
| `STALWART_WEBHOOK_SECRET` | Shared secret for webhook auth | — |

### Optional

| Var | Description | Default |
|-----|-------------|---------|
| `PORT` | HTTP listen port | `8080` |
| `DATABASE_URL` | CockroachDB connection string | cluster-internal default |
| `DISCORD_WEBHOOK_URL` | Discord webhook for notifications | — |

## Deployment

Push to `main` triggers GitHub Actions → builds Docker image → pushes to `ghcr.io/christmas-island/email-handler`.

Managed by ArgoCD. K8s manifests in `cluster-service/k8s.yaml`.

### Stalwart Webhook Configuration

Configure Stalwart to send webhooks to the email-handler:

1. In Stalwart admin (`https://mail.only-claws.net/`), go to **Settings → Webhooks**
2. Add a new webhook:
   - **URL:** `http://email-handler.socials.svc.cluster.local:8080/email/stalwart-webhook`
   - **Events:** `message-ingest.*`
   - **Auth:** Bearer token matching `STALWART_WEBHOOK_SECRET`

### Stalwart API Token

Create an API key in Stalwart admin for JMAP access:

1. Go to **Settings → API Keys** (or create a service account)
2. Generate a token with `jmap-email-get` permissions
3. Add to k8s secret: `kubectl -n socials create secret generic email-handler-secret --from-literal=stalwart-api-token=<token>`

## CF Worker (Legacy)

The Cloudflare Email Worker is kept as a fallback. To deploy:

```bash
cd .
wrangler deploy
wrangler secret put HANDLER_SECRET
```

### CF Worker Variables

| Var | Description |
|-----|-------------|
| `HANDLER_URL` | Cluster API endpoint |
| `HANDLER_SECRET` | Shared secret (set via `wrangler secret`) |
| `FALLBACK_EMAIL` | Gmail to forward to if API is down |
