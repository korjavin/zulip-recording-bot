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
// unrecorded, so a redelivery retries it. The watchdog can feed it an event
// rebuilt from GET /recordings/{id} the same way.
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

	prev := job
	var indicate, unindicate, transcribe bool
	var note string
	switch ev.Event {
	case evWaitingAdmission, evStarted:
		// A failed job here is one whose POST /recordings answer never arrived:
		// the recorder has it after all, so 🔴 comes back.
		indicate = job.State == jobFailed
		job.State, job.Error = jobRunning, ""
		// Meet is DM-only and its guest waits in the lobby until someone admits it.
		if ev.Event == evWaitingAdmission && job.Recorder == recorderMeet {
			note = fmt.Sprintf("Asking to join %s as a guest — admit %s from the lobby.", path.Base(job.URL), b.cfg.BotDisplayName)
		}
	case evFinished:
		job.State, job.Error = jobFinished, ""
		job.StartedAt, job.EndedAt, job.DurationS = ev.StartedAt, ev.EndedAt, ev.DurationS
		job.Participants, job.Artifacts = ev.Participants, ev.Artifacts
		unindicate = true
		if ev.DurationS < float64(b.cfg.MinRecordingS) {
			note = fmt.Sprintf("Recording too short (under %d s) — nothing to transcribe.", b.cfg.MinRecordingS)
		} else {
			transcribe = true
		}
	case evFailed:
		job.State, job.Error = jobFailed, ev.Error
		job.Artifacts = ev.Artifacts
		unindicate = true
		note = b.failureNote(job)
	default:
		slog.Warn("unknown recorder event ignored", "job", job.ID, "event", ev.Event)
		return nil
	}

	// The event is recorded before anyone is told, so a broken disk can never
	// turn every redelivery into another note. A transient Zulip failure rolls
	// the record back and the recorder's retry tries again.
	job.LastEvent = ev.Event
	job.Events = append(job.Events, ev.Event)
	if err := job.save(b.cfg.DataDir); err != nil {
		slog.Error("saving the job failed", "job", job.ID, "err", err)
		return err
	}
	err = nil
	if indicate {
		err = zulipStep(job, "adding the recording indicator", b.z.AddReaction(ctx, job.MessageID, recordingEmoji))
	}
	// 🔴 goes before the note: removing it again on a retry is harmless, a
	// second note is not.
	if err == nil && unindicate {
		err = zulipStep(job, "removing the recording indicator", b.z.RemoveReaction(ctx, job.MessageID, recordingEmoji))
	}
	if err == nil && note != "" {
		err = zulipStep(job, "posting to zulip", b.reply(ctx, job, note))
	}
	if err != nil {
		if serr := prev.save(b.cfg.DataDir); serr != nil {
			slog.Error("rolling the job back failed", "job", job.ID, "err", serr)
		}
		return err
	}
	if transcribe {
		b.handOff(job)
	}
	slog.Info("recorder event", "job", job.ID, "event", ev.Event, "state", job.State, "error", job.Error)
	return nil
}

// zulipStep sorts the outcome of one Zulip call made for an event. A refusal
// (a deleted message, a reaction already there or gone) is logged and the
// event goes on: repeating the call cannot help. Anything else is transient and
// comes back wrapped in errZulip.
func zulipStep(job Job, what string, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errRejected):
		slog.Warn(what+" was refused", "job", job.ID, "err", err)
		return nil
	default:
		slog.Error(what+" failed", "job", job.ID, "err", err)
		return fmt.Errorf("%w: %v", errZulip, err)
	}
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
