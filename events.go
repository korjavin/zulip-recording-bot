package main

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"regexp"
	"slices"
)

// Recorder event names (docs/architecture.md §3.5).
const (
	evWaitingAdmission = "recording.waiting_admission"
	evStarted          = "recording.started"
	evFinished         = "recording.finished"
	evFailed           = "recording.failed"
)

// jobIDRe is the contract's id format; anything else cannot be one of our jobs
// and must never reach a filesystem path.
var jobIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// maxEventBody caps a recorder event; real ones are a few KB.
const maxEventBody = 1 << 20

// recorderEvent is the body of every recorder event (§3.5).
type recorderEvent struct {
	Event        string     `json:"event"`
	ID           string     `json:"id"`
	Error        string     `json:"error,omitempty"`
	StartedAt    string     `json:"started_at,omitempty"`
	EndedAt      string     `json:"ended_at,omitempty"`
	DurationS    float64    `json:"duration_s,omitempty"`
	Participants []string   `json:"participants,omitempty"`
	Artifacts    []Artifact `json:"artifacts,omitempty"`
}

// errUnknownJob is an event for a job the bot never created.
var errUnknownJob = errors.New("unknown job")

// errZulip is a Zulip call that failed while handling an event; the event
// stays unhandled so the recorder's retry delivers it again.
var errZulip = errors.New("zulip call failed")

// serveEvents is POST /events: a signed recorder event.
func (b *Bot) serveEvents(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxEventBody))
	if err != nil {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	if !hmac.Equal([]byte(r.Header.Get("x-recorder-signature")), []byte(sign(body, b.cfg.RecorderSecret))) {
		slog.Warn("recorder event with a bad signature")
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	var ev recorderEvent
	if err := json.Unmarshal(body, &ev); err != nil || ev.Event == "" {
		http.Error(w, "bad event", http.StatusBadRequest)
		return
	}
	err = b.applyEvent(r.Context(), ev)
	switch {
	case errors.Is(err, errUnknownJob):
		http.Error(w, "unknown job", http.StatusNotFound)
	case errors.Is(err, errZulip):
		http.Error(w, "zulip call failed", http.StatusBadGateway)
	case err != nil:
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// applyEvent acts on one recorder event and records it in the job. A repeated
// (id, event) is a no-op. Errors other than errUnknownJob leave the event
// unrecorded, so a redelivery retries it.
//
// ponytail: Bot.mu is held across the Zulip calls, so events and claims are
// serialised bot-wide; per-job locks if that ever queues up.
func (b *Bot) applyEvent(ctx context.Context, ev recorderEvent) error {
	if !jobIDRe.MatchString(ev.ID) {
		return errUnknownJob
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	job, err := loadJob(b.cfg.DataDir, ev.ID)
	if errors.Is(err, os.ErrNotExist) {
		slog.Warn("recorder event for an unknown job", "job", ev.ID, "event", ev.Event)
		return errUnknownJob
	}
	if err != nil {
		slog.Error("reading the job failed", "job", ev.ID, "err", err)
		return err
	}
	if slices.Contains(job.Events, ev.Event) {
		slog.Debug("duplicate recorder event", "job", job.ID, "event", ev.Event)
		return nil
	}
	// A late best-effort event must not reopen a job the recorder already ended.
	ended := job.State == jobFinished || (job.State == jobFailed && job.LastEvent != "")
	if ended && (ev.Event == evWaitingAdmission || ev.Event == evStarted) {
		slog.Debug("progress event after the job ended", "job", job.ID, "event", ev.Event)
		return nil
	}

	switch ev.Event {
	case evWaitingAdmission:
		// Meet is DM-only and its guest waits in the lobby until someone admits it.
		if job.Recorder == recorderMeet {
			note := fmt.Sprintf("Asking to join %s as a guest — admit %s from the lobby.", path.Base(job.URL), b.cfg.BotDisplayName)
			if err := b.notify(ctx, job, note); err != nil {
				return err
			}
		}
		job.State = jobRunning
	case evStarted:
		job.State = jobRunning // 🔴 is already on
	case evFinished:
		// 🔴 goes first: removing it again on a redelivery is a no-op, a second
		// note is not.
		if err := b.removeIndicator(ctx, job); err != nil {
			return err
		}
		job.State, job.Error = jobFinished, ""
		job.StartedAt, job.EndedAt, job.DurationS = ev.StartedAt, ev.EndedAt, ev.DurationS
		job.Participants, job.Artifacts = ev.Participants, ev.Artifacts
		if ev.DurationS < float64(b.cfg.MinRecordingS) {
			note := fmt.Sprintf("Recording too short (under %d s) — nothing to transcribe.", b.cfg.MinRecordingS)
			if err := b.notify(ctx, job, note); err != nil {
				return err
			}
		} else {
			b.handOff(job)
		}
	case evFailed:
		if err := b.removeIndicator(ctx, job); err != nil {
			return err
		}
		job.State, job.Error = jobFailed, ev.Error
		job.Artifacts = ev.Artifacts
		if err := b.notify(ctx, job, b.failureNote(job)); err != nil {
			return err
		}
	default:
		slog.Warn("unknown recorder event ignored", "job", job.ID, "event", ev.Event)
		return nil
	}

	job.LastEvent = ev.Event
	job.Events = append(job.Events, ev.Event)
	if err := job.save(b.cfg.DataDir); err != nil {
		slog.Error("saving the job failed", "job", job.ID, "err", err)
		return err
	}
	slog.Info("recorder event", "job", job.ID, "event", ev.Event, "state", job.State, "error", job.Error)
	return nil
}

// failureNote is the one line a recording.failed event gets. A partial
// recording is mentioned by job id only — never by file path.
func (b *Bot) failureNote(job Job) string {
	var what, outcome string
	switch job.Error {
	case "not_admitted":
		what, outcome = b.cfg.BotDisplayName+" was not admitted to the call (or nobody joined)", "nothing recorded."
	case "interrupted":
		what, outcome = "Recording was interrupted by a service restart", "no transcript."
	default: // recorder_failed, or an error this bot does not know yet
		what, outcome = "Recording failed (recorder error)", "nothing recorded."
	}
	if len(job.Artifacts) > 0 {
		outcome = "a partial recording was kept (job " + job.ID + "), no transcript."
	}
	return what + " — " + outcome
}

// handOff passes a finished recording to the transcriber.
//
// ponytail: a stub until the transcriber hand-off lands; the job record already
// holds everything §5 needs.
func (b *Bot) handOff(job Job) {
	slog.Info("recording ready for transcription", "job", job.ID, "duration_s", job.DurationS)
}

// notify posts content for the job and wraps a failure in errZulip.
func (b *Bot) notify(ctx context.Context, job Job, content string) error {
	if err := b.reply(ctx, job, content); err != nil {
		slog.Error("posting to zulip failed", "job", job.ID, "err", err)
		return fmt.Errorf("%w: %v", errZulip, err)
	}
	return nil
}

// removeIndicator drops 🔴 and wraps a failure in errZulip. A reaction that is
// already gone counts as removed.
func (b *Bot) removeIndicator(ctx context.Context, job Job) error {
	if err := b.z.RemoveReaction(ctx, job.MessageID, recordingEmoji); err != nil {
		slog.Error("removing the recording indicator failed", "job", job.ID, "err", err)
		return fmt.Errorf("%w: %v", errZulip, err)
	}
	return nil
}
