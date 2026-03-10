# 📧 email-handler

Inbound email handler for `*@only-claws.net` — Cloudflare Email Worker + cluster service.

## Architecture

```
*@only-claws.net → Cloudflare Email Routing → Email Worker → POST /email/inbound → email-handler (socials namespace)
                                                    ↓ (fallback)
                                              forward to Gmail
```

## Components

### Cloudflare Email Worker (`worker.js`)
- Catches all `*@only-claws.net` emails
- POSTs JSON payload to cluster API
- Falls back to forwarding to Christmas Island Gmail if API is down

### Cluster Service (`cluster-service/`)
- Go HTTP server deployed in `socials` namespace
- `POST /email/inbound` — receives emails from CF Worker
- `GET /email/query/{clawname}` — claws check their inbox for OTPs
- `GET /health` — liveness/readiness probe
- Extracts OTP codes and verification links via regex

## Deployment

### CI
Push to `main` triggers GitHub Actions → builds Docker image → pushes to `ghcr.io/christmas-island/email-handler`.

### Cluster
Managed by ArgoCD. K8s manifests in `cluster-service/k8s.yaml`.

### Cloudflare Worker
```bash
cd .
wrangler deploy
wrangler secret put HANDLER_SECRET
```

## Environment Variables

### Cluster Service
| Var | Description |
|-----|-------------|
| `PORT` | HTTP listen port (default: 8080) |
| `HANDLER_SECRET` | Shared secret for Worker → API auth |
| `DISCORD_WEBHOOK_URL` | (optional) Post notifications to Discord |

### Cloudflare Worker
| Var | Description |
|-----|-------------|
| `HANDLER_URL` | Cluster API endpoint |
| `HANDLER_SECRET` | Shared secret (set via `wrangler secret`) |
| `FALLBACK_EMAIL` | Gmail to forward to if API is down |
