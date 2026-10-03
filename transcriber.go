package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// Job.Webhook values: a finished recording waits as "pending" until the
// transcriber answers 2xx, then it is "sent".
const (
	handOffPending = "pending"
	handOffSent    = "sent"
)

// handOffSweep is how often unsent hand-offs are retried after the per-job
// retries ran out.
const handOffSweep = time.Hour

// handOffRetryDelays are the pauses between attempts of one hand-off (§5,
// like §3.5). A var so tests can shrink it.
var handOffRetryDelays = []time.Duration{5 * time.Second, 15 * time.Second, 45 * time.Second, 2 * time.Minute, 5 * time.Minute}

var transcriberHTTP = &http.Client{
	Timeout: 30 * time.Second,
	// A followed redirect turns into a GET without the signed body.
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// handOffBody is the transcriber's inbound shape (docs/architecture.md §5).
type handOffBody struct {
	Event        string   `json:"event"`
	ID           string   `json:"id"`
	MessageID    int64    `json:"message_id"`
	Stream       string   `json:"stream"`
	Topic        string   `json:"topic"`
	DMUserID     int64    `json:"dm_user_id,omitempty"`
	JitsiURL     string   `json:"jitsi_url"` // any platform; the name is the transcriber's
	Source       string   `json:"source,omitempty"`
	AudioPath    string   `json:"audio_path"`
	DurationS    float64  `json:"duration_s"`
	StartedAt    string   `json:"started_at"`
	EndedAt      string   `json:"ended_at"`
	Participants []string `json:"participants"`
	CallbackURL  string   `json:"callback_url"`
	Tracks       []track  `json:"tracks,omitempty"`
	SpeakerHints string   `json:"speaker_hints_path,omitempty"`
}

type track struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	Path    string  `json:"path"`
	OffsetS float64 `json:"offset_s"`
	EndedS  float64 `json:"ended_s"`
}

func (b *Bot) handOffBody(job Job) handOffBody {
	h := handOffBody{
		Event:        evFinished,
		ID:           job.ID,
		MessageID:    job.MessageID,
		Stream:       job.Stream,
		Topic:        job.Topic,
		DMUserID:     job.DMUserID,
		JitsiURL:     job.URL,
		DurationS:    job.DurationS,
		StartedAt:    job.StartedAt,
		EndedAt:      job.EndedAt,
		Participants: job.Participants,
		CallbackURL:  b.cfg.PublicURL + "/notify",
	}
	if job.Recorder == recorderMeet {
		h.Source = recorderMeet
	}
	if h.Participants == nil {
		h.Participants = []string{} // never null: the transcriber ranges over it
	}
	for _, a := range job.Artifacts {
		switch a.Kind {
		case "audio":
			h.AudioPath = a.Path
		case "track":
			h.Tracks = append(h.Tracks, track{ID: a.ParticipantID, Name: a.Name, Path: a.Path, OffsetS: a.OffsetS, EndedS: a.EndedS})
		case "captions":
			h.SpeakerHints = a.Path
		}
	}
	return h
}

// kickHandOff wakes the hand-off loop without waiting for it; a wake-up already
// queued covers this one too.
func (b *Bot) kickHandOff() {
	select {
	case b.kick <- struct{}{}:
	default:
	}
}

// handOffLoop delivers pending hand-offs at startup, whenever a recording
// finishes, and hourly, until ctx ends.
func (b *Bot) handOffLoop(ctx context.Context) {
	if b.cfg.WebhookURL == "" {
		slog.Info("WEBHOOK_URL is empty: finished recordings are not handed to a transcriber")
		return
	}
	t := time.NewTicker(handOffSweep)
	defer t.Stop()
	for {
		b.sweepHandOffs(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-b.kick:
		}
	}
}

// sweepHandOffs sends every pending hand-off. A failed one stays pending for the
// next sweep and the sweep goes on, so one bad job never holds up the others.
//
// ponytail: with the transcriber down, every pending job runs its full retry
// table in turn (~8 min each); skip retries after the first outage in a sweep
// if a backlog ever makes that slow.
func (b *Bot) sweepHandOffs(ctx context.Context) {
	entries, err := os.ReadDir(filepath.Join(b.cfg.DataDir, "jobs"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Error("listing jobs failed", "err", err)
		return
	}
	for _, e := range entries {
		job, err := loadJob(b.cfg.DataDir, e.Name())
		if err != nil || job.Webhook != handOffPending {
			continue
		}
		err = b.handOff(ctx, job)
		if err != nil && ctx.Err() == nil {
			slog.Error("transcriber hand-off failed, retrying in the next sweep", "job", job.ID, "err", err)
		}
	}
}

// handOff sends one job to the transcriber, retrying a network error or a 5xx
// per handOffRetryDelays, and marks it sent. The transcriber is idempotent on id, so a repeat after a
// crash between the send and the save is harmless.
func (b *Bot) handOff(ctx context.Context, job Job) error {
	body, err := json.Marshal(b.handOffBody(job))
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		err = postHandOff(ctx, b.cfg.WebhookURL, b.cfg.WebhookSecret, body)
		if err == nil {
			break
		}
		if !errors.Is(err, errTranscriberDown) || attempt == len(handOffRetryDelays) || ctx.Err() != nil {
			return err
		}
		slog.Warn("transcriber hand-off failed, retrying", "job", job.ID, "attempt", attempt+1, "err", err)
		pause(ctx, handOffRetryDelays[attempt])
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	cur, err := loadJob(b.cfg.DataDir, job.ID)
	if err != nil {
		return err
	}
	cur.Webhook = handOffSent
	if err := cur.save(b.cfg.DataDir); err != nil {
		return err
	}
	slog.Info("recording handed to the transcriber", "job", job.ID)
	return nil
}

// errTranscriberDown is a network error or a 5xx: worth retrying. A 4xx is a
// refusal of this job and is not retried at once.
var errTranscriberDown = errors.New("transcriber unavailable")

// postHandOff is one attempt; anything but 2xx is an error.
func postHandOff(ctx context.Context, target, secret string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return errors.New("WEBHOOK_URL is not a valid URL") // the parse error would quote it
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-jitsi-capture-event", evFinished)
	req.Header.Set("x-jitsi-capture-signature", sign(body, secret))
	resp, err := transcriberHTTP.Do(req)
	var uerr *url.Error
	if errors.As(err, &uerr) {
		err = uerr.Err // the url.Error text carries WEBHOOK_URL, which may hold a credential
	}
	if err != nil {
		return fmt.Errorf("%w: WEBHOOK_URL: %v", errTranscriberDown, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("%w: http %d", errTranscriberDown, resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("transcriber refused the hand-off: http %d", resp.StatusCode)
	}
	return nil
}

// serveNotify is POST /notify: the transcript pipeline's signed callback, whose
// content is posted verbatim where the recording was requested.
func (b *Bot) serveNotify(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxEventBody))
	if err != nil {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	// No secret means no trust: an unset WEBHOOK_SECRET refuses every call.
	if b.cfg.WebhookSecret == "" ||
		!hmac.Equal([]byte(r.Header.Get("x-jitsi-capture-signature")), []byte(sign(body, b.cfg.WebhookSecret))) {
		slog.Warn("notify with a bad signature")
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	var n struct {
		ID      string `json:"id"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(body, &n); err != nil || n.ID == "" || n.Content == "" {
		http.Error(w, "missing id or content", http.StatusBadRequest)
		return
	}
	var job Job
	err = os.ErrNotExist
	if jobIDRe.MatchString(n.ID) { // checked before the id becomes a path
		job, err = loadJob(b.cfg.DataDir, n.ID)
	}
	if errors.Is(err, os.ErrNotExist) {
		slog.Warn("notify for an unknown job", "job", n.ID)
		http.Error(w, "unknown job", http.StatusNotFound)
		return
	}
	if err != nil {
		slog.Error("reading the job failed", "job", n.ID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := b.reply(r.Context(), job, n.Content); err != nil {
		slog.Error("posting the notification failed", "job", job.ID, "err", err)
		http.Error(w, "zulip refused the message", http.StatusBadGateway)
		return
	}
	slog.Info("notification posted", "job", job.ID)
}
