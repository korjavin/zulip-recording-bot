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

`.env.example` lists every variable with a placeholder. `DOMAIN`,
`TRAEFIK_NETWORK_NAME` and `TRAEFIK_CERTRESOLVER` are read by
`docker-compose.yml`, not by the bot. The stack does not pass `LISTEN_ADDR` or
`DATA_DIR`: the image fixes them (`:8080`, `/data`, the only path the container
user owns).

`PUBLIC_URL` is the base of both callbacks: recorders post to
`PUBLIC_URL/events`, tr2outline to `PUBLIC_URL/notify`. `/events` is not
published through Traefik, so use the container name on the shared network:
`http://zulip-recording-bot:8080`.

## Zulip setup

1. Settings → Personal → Bots → Add a new bot, type **Generic**. Its email and
   API key are `ZULIP_BOT_EMAIL` / `ZULIP_BOT_API_KEY`.
2. Subscribe the bot to every stream whose calls should be recordable. It only
   sees messages in streams it is subscribed to, and DMs sent to it.

## Usage

* **Stream:** a message with a Jitsi link (under `JITSI_BASE_URL`) gets a 🎙️
  reaction from the bot. Anyone clicks 🎙️ to record. 🔴 is added to the message
  while the recording runs and removed when it ends.
* **DM:** send the bot a Jitsi or Google Meet link to record right away; it
  answers in the DM. Meet links are honoured in DMs only. A Meet recorder joins
  as a guest: the bot says "Asking to join … — admit NoteTaker from the lobby",
  and someone in the call has to admit it.
* **Outcome**, posted in the same topic or DM:
  * a finished recording goes to the transcriber; when the transcript is
    published, "Transcript ready: <link>" arrives;
  * shorter than `MIN_RECORDING_S` → "Recording too short — nothing to
    transcribe.";
  * not admitted, recorder error, a recorder restart or a recorder that stopped
    answering → one line saying so, and no transcript.

## Endpoints

| endpoint | caller | auth |
|---|---|---|
| `GET /health` | Traefik, smoke tests | none; `200 {"status":"ok"}` |
| `POST /events` | recorders, over the shared network | `x-recorder-signature` (`RECORDER_SECRET`) |
| `POST /notify` | tr2outline | `x-jitsi-capture-signature` (`WEBHOOK_SECRET`) |

Traefik publishes only `/notify` and `/health` on `DOMAIN`. Contracts:
[docs/architecture.md](docs/architecture.md) §3 and §5.

## Deploy

`docker-compose.yml` runs one container with its own named volume for job
state at `/data` (no audio — the bot never mounts the recordings volume) on
the external network `${TRAEFIK_NETWORK_NAME:-traefik}`, which the recorders,
the transcriber and tr2outline share.

`.github/workflows/deploy.yml`, on every push to `master`:

1. builds and pushes `ghcr.io/korjavin/zulip-recording-bot:<sha>`;
2. force-pushes the `deploy` branch: `master` with the image line in
   `docker-compose.yml` pinned to that sha;
3. calls the `PORTAINER_REDEPLOY_HOOK` repository secret, when set.

Portainer runs the repo as a git stack on the `deploy` branch, with the
variables from `.env.example` in the stack environment. Locally:
`cp .env.example .env`, edit, `docker compose up -d`.

### One-time owner setup

* Repository secret `PORTAINER_REDEPLOY_HOOK`: the stack's webhook URL from
  Portainer (stack → GitOps updates → Webhook). Without it the workflow still
  builds and updates `deploy`; Portainer then picks the change up on its own
  polling, if enabled.
* GHCR: after the first build, make the package public, or give Portainer a
  registry credential for `ghcr.io`.
* Generate `RECORDER_SECRET` (`openssl rand -hex 32`); `WEBHOOK_SECRET` is the
  one the transcriber and tr2outline already use.

## Rollout runbook

Do each step in order; each has a check before you move on.

**1. Recorders.** Deploy `jitsi-recorder` and `meet-recorder` on the shared
network, each mounting the recordings volume (`docker volume create
recordings` once) at `/data`, with the same `RECORDER_SECRET`. Container names
must match `JITSI_RECORDER_URL` / `MEET_RECORDER_URL` (defaults
`jitsi-recorder`, `meet-recorder`, port 8080).

**2. Smoke-test each recorder** from a throwaway container on the network.
`GET` signs the empty body; an unknown id answers `404` only when the signature
is accepted (a bad one is `401`):

```bash
read -rs RECORDER_SECRET     # paste the real one; stays out of shell history
SIG="sha256=$(printf '' | openssl dgst -sha256 -hmac "$RECORDER_SECRET" | sed 's/^.* //')"
for r in jitsi-recorder meet-recorder; do
  docker run --rm --network traefik curlimages/curl -s "http://$r:8080/health"; echo
  docker run --rm --network traefik curlimages/curl -s -o /dev/null -w '%{http_code}\n' \
    -H "x-recorder-signature: $SIG" "http://$r:8080/recordings/smoke-test"   # expect 404
done
```

**3. This stack.** Deploy it with `JITSI_RECORDER_URL`, `MEET_RECORDER_URL`,
`RECORDER_SECRET`, `PUBLIC_URL=http://zulip-recording-bot:8080`,
`WEBHOOK_URL` (the transcriber's inbound URL) and `WEBHOOK_SECRET`. Check:
the log shows `zulip event queue registered`, and
`curl https://recording-bot.example.com/health` (your `DOMAIN`) answers `{"status":"ok"}`.

**4. Zulip smoke test.**
* Post a Jitsi link (e.g. `https://meet.jit.si/SomeRoom`) in a subscribed
  stream, join the room yourself, click 🎙️. Expect 🔴 within a minute; talk
  for longer than `MIN_RECORDING_S`, leave; 🔴 goes away after `EMPTY_GRACE_S`.
* DM the bot a Google Meet link of a call you are in. Expect the lobby note,
  admit the bot, talk, leave.

**5. Transcript.** The transcriber log shows `recording.finished` for the
message id; once tr2outline publishes, "Transcript ready: <link>" lands in the
stream topic (and in the DM for the Meet test). Nothing arrives → the bot log
says `recording handed to the transcriber` or why not (`WEBHOOK_URL` /
`WEBHOOK_SECRET`); `notify with a bad signature` means tr2outline's secret
differs; no `notification posted` at all means tr2outline cannot reach
`PUBLIC_URL/notify`.

Rollback: revert on `master` (the workflow redeploys), or pin the previous sha
on the `deploy` branch's image line and redeploy the stack. Job state in the
volume survives.

## Build and test

```bash
gofmt -l . && go vet ./... && go test -race ./...
docker build -t zulip-recording-bot .
```
