package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// shutdownGrace is what the HTTP server gets to finish in-flight requests.
const shutdownGrace = 5 * time.Second

func main() {
	cfg, err := loadConfig()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel(cfg.LogLevel)})))
	if err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
	slog.Info("zulip-recording-bot starting", "listen_addr", cfg.ListenAddr, "data_dir", cfg.DataDir)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		// Bad Zulip credentials stop the service instead of leaving it half-up.
		if err := newBot(cfg, newZulip(cfg)).Run(ctx); err != nil {
			slog.Error("zulip-recording-bot failed", "err", err)
			os.Exit(1)
		}
	}()
	if err := run(ctx, cfg, nil); err != nil {
		slog.Error("zulip-recording-bot failed", "err", err)
		os.Exit(1)
	}
	slog.Info("zulip-recording-bot stopped")
}

// run serves HTTP until ctx ends. ready, when set, receives the listener's
// address once it is up — the seam a test needs to address a :0 port; main
// passes nil.
func run(ctx context.Context, cfg Config, ready func(net.Addr)) error {
	srv := newHTTPServer(cfg.ListenAddr, handler())
	// Listen before serving so a busy port is a startup failure, not a log line.
	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return err
	}
	if ready != nil {
		ready(ln.Addr())
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		return err
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// logLevel parses LOG_LEVEL; anything unrecognised falls back to INFO.
func logLevel(s string) slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return slog.LevelInfo
	}
	return l
}
