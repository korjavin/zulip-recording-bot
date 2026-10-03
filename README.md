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
| `LISTEN_ADDR` | `:8080` | HTTP listener (`GET /health`, `POST /events`, `POST /notify`) |
| `PUBLIC_URL` | `http://localhost:8080` | base of the callback URLs given to other services |
| `DATA_DIR` | `/data` | the bot's own job state |
| `LOG_LEVEL` | `INFO` | `DEBUG`, `INFO`, `WARN`, `ERROR` |
| `RECORDER_SECRET` | — | required; HMAC key for `x-recorder-signature` |
| `JITSI_BASE_URL` | `https://meet.jit.si` | links under it are Jitsi calls |
| `JITSI_RECORDER_URL` | `http://jitsi-recorder:8080` | |
| `MEET_RECORDER_URL` | `http://meet-recorder:8080` | |
| `BOT_DISPLAY_NAME` | `NoteTaker` | the name the recorder joins with |
| `JOIN_TIMEOUT_S` | `600` | Jitsi: give up if not admitted by then |
| `MEET_JOIN_TIMEOUT_S` | `1200` | Meet: same, for the lobby |
| `MAX_DURATION_S` | `14400` | longest recording |
| `EMPTY_GRACE_S` | `60` | stop after the room has been empty this long |
| `MIN_RECORDING_S` | `15` | a shorter recording is not transcribed |
| `WEBHOOK_URL` | — | the transcriber's inbound URL; empty disables the hand-off |
| `WEBHOOK_SECRET` | — | required with `WEBHOOK_URL`; signs the hand-off and verifies `POST /notify` |

## Usage

* A stream message with a Jitsi link gets a 🎙️ reaction. Click it to record;
  🔴 shows while the recording runs.
* Send the bot a Jitsi or Google Meet link by DM to record right away. Meet
  links are honoured in DMs only.

## Build and test

```bash
gofmt -l . && go vet ./... && go test -race ./...
docker build -t zulip-recording-bot .
```
