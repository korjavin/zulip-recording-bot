FROM golang:1.27-alpine AS build
WORKDIR /src
# ponytail: no go.sum, the service is stdlib-only; copy it here the day a
# dependency appears.
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /zulip-recording-bot .

# distroless/static carries CA certificates and a nonroot user; no shell, no
# browser, no audio tooling — the bot never touches audio.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /zulip-recording-bot /zulip-recording-bot
ENV LISTEN_ADDR=:8080 DATA_DIR=/data
EXPOSE 8080
ENTRYPOINT ["/zulip-recording-bot"]
