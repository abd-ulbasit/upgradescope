# Webhook payload

`upgradescope serve --webhook <url>` POSTs one JSON notification per
cluster and evaluation pass that changed its readiness. When it is sent,
how delivery is retried and how changes are grouped are in
[Running the server](../operations.md#notifications).

The payload has a JSON Schema,
[`api/webhook.schema.json`](https://github.com/abd-ulbasit/upgradescope/blob/main/api/webhook.schema.json)
(draft 2020-12). It allows fields it does not list, because fields may be
added within a `schemaVersion`; ignore the ones you do not know. A test
delivers real notifications through ingest, the outbox and a signed
request and validates them against it, so the schema and this page
cannot drift from what the server sends.

## Payload (schemaVersion 1)

`POST` with `Content-Type: application/json`:

```json
{
  "schemaVersion": 1,
  "deliveryId": "3f2b8c1e9a7d4e6f8b0c2d4e6f8a0b1c",
  "type": "readiness.changed",
  "timestamp": "2026-10-02T09:30:00Z",
  "cluster": { "id": 7, "name": "prod-eu-1" },
  "targets": [
    { "target": "1.35", "verdict": "blocked", "score": 75, "blockers": 1 },
    { "target": "1.36", "verdict": "blocked", "score": 50, "blockers": 2 }
  ],
  "changes": [
    {
      "kind": "new-blocker",
      "key": "eol-addon/ingress-nginx",
      "severity": "blocker",
      "title": "ingress-nginx is end-of-life",
      "detail": "retired upstream in March 2026",
      "targets": ["1.35", "1.36"]
    },
    {
      "kind": "eol-approaching",
      "key": "eol-approaching/istio",
      "severity": "warning",
      "title": "Istio 1.24 reaches end-of-life on 2026-11-30",
      "targets": ["1.36"]
    }
  ],
  "omitted": { "new-blocker": 3 }
}
```

| Field | Meaning |
|---|---|
| `schemaVersion` | `1`. Fields may be added within a version; a rename or removal bumps it. |
| `deliveryId` | 32 hex characters, the same on every retry and for every sink. |
| `type` | `readiness.changed`, the only type so far. |
| `timestamp` | When the evaluation pass ran, RFC 3339 UTC. |
| `cluster` | `id` (the API's cluster id) and `name`. |
| `targets[]` | The verdict after the pass (`ready`, `blocked` or `unknown`), score and blocker count of every target with a change. |
| `changes[]` | `kind`: `new-blocker`, `became-ready` or `eol-approaching`. `key`: the finding's stable key (none for `became-ready`); `severity`: `blocker` or `warning`; `title`, `detail`; `targets`: the targets the change applies to. Ordered new-blocker, became-ready, eol-approaching. |
| `omitted` | Changes left out by the per-kind cap, by kind. Absent when nothing was left out. |

Headers: `X-Upgradescope-Delivery` (the delivery id),
`X-Upgradescope-Event` (the type) and, with a secret,
`X-Upgradescope-Signature`.

Messages queued by a server older than this format are delivered after an
upgrade as one-change notifications with a `deliveryId` of `outbox-<n>`.

## Verifying the signature

`serve --webhook-secret` (`$UPGRADESCOPE_WEBHOOK_SECRET`,
`--webhook-secret-file`; chart `server.webhookSecret`) signs every webhook
request:

```
X-Upgradescope-Signature: sha256=<hex HMAC-SHA256(secret, raw request body)>
```

Compute the HMAC over the raw body bytes, before any JSON parsing, and
compare in constant time:

```go
mac := hmac.New(sha256.New, []byte(secret))
mac.Write(body)
want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
ok := hmac.Equal([]byte(want), []byte(r.Header.Get("X-Upgradescope-Signature")))
```

```sh
printf '%s' "$BODY" | openssl dgst -sha256 -hmac "$SECRET" | sed 's/^.* /sha256=/'
```

The signature covers the body only; reject old `timestamp`s and repeated
`deliveryId`s to resist replay.
