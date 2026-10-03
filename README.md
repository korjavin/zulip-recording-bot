# zulip-recording-bot

A Zulip bot that records calls on request. It notices call links in Zulip,
asks the matching recorder service to record the call, keeps the people who
asked informed, hands finished recordings to the transcriber and posts
"transcript ready" back where the request came from.

The bot never touches audio itself. The design, the services around it and the
recorder contract are in [docs/architecture.md](docs/architecture.md).

## Configuration

Environment only:

| variable | default | |
|---|---|---|
| `ZULIP_SITE` | — | required, e.g. `https://zulip.example.com` |
| `ZULIP_BOT_EMAIL` | — | required |
| `ZULIP_BOT_API_KEY` | — | required |
| `LISTEN_ADDR` | `:8080` | HTTP listener (`GET /health`) |
| `PUBLIC_URL` | `http://localhost:8080` | base of the callback URLs given to other services |
| `DATA_DIR` | `/data` | the bot's own job state |
| `LOG_LEVEL` | `INFO` | `DEBUG`, `INFO`, `WARN`, `ERROR` |

## Build and test

```bash
gofmt -l . && go vet ./... && go test -race ./...
docker build -t zulip-recording-bot .
```
