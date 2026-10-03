package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
)

// run serves /health on its listener and returns cleanly when ctx ends.
func TestRunServesHealthAndShutsDown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	addrc := make(chan net.Addr, 1)
	done := make(chan error, 1)
	cfg := Config{ListenAddr: "127.0.0.1:0"}
	go func() { done <- run(ctx, cfg, newBot(cfg, nil), func(a net.Addr) { addrc <- a }) }()

	resp, err := http.Get("http://" + (<-addrc).String() + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != `{"status":"ok"}`+"\n" {
		t.Errorf("GET /health = %d %q", resp.StatusCode, body)
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("run() = %v; want nil after cancel", err)
	}
}

func TestLogLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{"DEBUG": slog.LevelDebug, "warn": slog.LevelWarn, "": slog.LevelInfo, "bogus": slog.LevelInfo} {
		if got := logLevel(in); got != want {
			t.Errorf("logLevel(%q) = %v; want %v", in, got, want)
		}
	}
}
