package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// retryDelay is the pause after a failed register or events call.
const retryDelay = 5 * time.Second

// recordingEmoji marks a message whose call is being recorded.
const recordingEmoji = "red_circle"

// noteRecorderFailed is posted when no recorder took the job.
const noteRecorderFailed = "Recording failed (recorder error) — nothing recorded."

// watchdogSlack is added to a job's timeouts to get its deadline (§4).
const watchdogSlack = 10 * time.Minute

// meetRe matches a Google Meet link and captures its meeting code. The job URL
// is rebuilt from the code alone, so a query string (authuser, ...) never
// reaches the recorder, the job record or the logs.
var meetRe = regexp.MustCompile(`https://meet\.google\.com/([a-z]{3}-[a-z]{4}-[a-z]{3})\b`)

// Bot is the Zulip event loop. A stream message with a Jitsi link gets a 🎙️
// reaction; a human clicking it starts a recording. A Jitsi or Meet link sent
// by DM is an explicit request and starts one right away.
type Bot struct {
	cfg     Config
	z       *Zulip
	botID   int64
	jitsiRe *regexp.Regexp

	mu sync.Mutex     // serialises the check-and-claim of a job id
	wg sync.WaitGroup // in-flight recorder requests and the hand-off loop

	kick chan struct{} // wakes the hand-off loop when a recording is ready
}

func newBot(cfg Config, z *Zulip) *Bot {
	return &Bot{
		cfg:  cfg,
		z:    z,
		kick: make(chan struct{}, 1),
		// Zulip's call button posts "[Join video call.](<base>/<room>)", so the raw
		// content is enough. The room runs to the first character markdown or prose
		// can put after it; "?" and "#" end it too, so a JWT or room password never
		// reaches the job record or the logs.
		jitsiRe: regexp.MustCompile(regexp.QuoteMeta(cfg.JitsiBaseURL) + "/[^\\s<>()\\[\\]{}\"'`?#|]+"),
	}
}

// Run drives the event loop until ctx ends. Only a failure to identify the bot
// is fatal — bad credentials should stop the service at startup rather than
// spin; everything after that is logged and retried.
func (b *Bot) Run(ctx context.Context) error {
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		b.handOffLoop(ctx)
	}()
	id, err := b.z.Me(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("zulip identity: %w", err)
	}
	b.botID = id
	slog.Info("zulip bot connected", "bot_user_id", id)

	var queueID string
	var lastEventID int64
	for ctx.Err() == nil {
		if queueID == "" {
			queueID, lastEventID, err = b.z.Register(ctx)
			if err != nil {
				if ctx.Err() == nil {
					slog.Error("zulip register failed", "err", err)
					pause(ctx, retryDelay)
				}
				continue
			}
			slog.Info("zulip event queue registered", "last_event_id", lastEventID)
		}
		events, err := b.z.Events(ctx, queueID, lastEventID)
		switch {
		case errors.Is(err, errBadQueue):
			slog.Warn("zulip event queue expired, re-registering")
			queueID = ""
			continue
		case err != nil:
			if ctx.Err() == nil {
				slog.Error("zulip events failed", "err", err)
				pause(ctx, retryDelay)
			}
			continue
		}
		for _, ev := range events {
			lastEventID = max(lastEventID, ev.ID)
			b.handle(ctx, ev)
		}
	}
	b.wg.Wait()
	return nil
}

// handle acts on one event. Failures are logged, never returned: one bad
// message must not take the loop down.
func (b *Bot) handle(ctx context.Context, ev Event) {
	switch {
	case ev.Type == "message" && ev.Message != nil && ev.Message.SenderID != b.botID:
		m := *ev.Message
		if m.Type == "private" {
			// Meet is DM-only: a Meet link in a stream gets nothing.
			if code := meetRe.FindStringSubmatch(m.Content); code != nil {
				b.start(ctx, m, recorderMeet, "https://meet.google.com/"+code[1])
			} else if u := b.jitsiURL(m.Content); u != "" {
				b.start(ctx, m, recorderJitsi, u)
			}
			return
		}
		if m.Type != "stream" || b.jitsiURL(m.Content) == "" {
			return
		}
		// Offer the recording silently — a reaction, never a text message.
		if err := b.z.AddReaction(ctx, m.ID, micEmoji); err != nil {
			slog.Error("adding the microphone reaction failed", "message_id", m.ID, "err", err)
			return
		}
		slog.Info("call link offered", "message_id", m.ID)
	case ev.Type == "reaction" && ev.Op == "add" && ev.EmojiName == micEmoji && ev.UserID != b.botID:
		// The reaction event carries no content, so the message is fetched.
		m, err := b.z.GetMessage(ctx, ev.MessageID)
		if err != nil {
			slog.Error("fetching the reacted message failed", "message_id", ev.MessageID, "err", err)
			return
		}
		if u := b.jitsiURL(m.Content); m.Type == "stream" && u != "" {
			b.start(ctx, m, recorderJitsi, u)
		}
	}
}

// jitsiURL returns the first Jitsi room link in content, or "".
func (b *Bot) jitsiURL(content string) string {
	// ponytail: sentence punctuation right after a pasted link is trimmed, so a
	// room literally named "standup." is unreachable from prose.
	u := strings.TrimRight(b.jitsiRe.FindString(content), ".,;:!")
	if strings.HasSuffix(u, "/") {
		return ""
	}
	return u
}

// start claims the job for message m and asks the recorder in the background,
// so a slow recorder never stalls the event loop. A job already in flight is a
// silent no-op; only a job no recorder ever accepted may be started again.
func (b *Bot) start(ctx context.Context, m Message, recorder, url string) {
	id := strconv.FormatInt(m.ID, 10)
	b.mu.Lock()
	old, err := loadJob(b.cfg.DataDir, id)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		b.mu.Unlock()
		slog.Error("reading the job failed", "job", id, "err", err)
		return
	case !(old.State == jobFailed && old.LastEvent == ""):
		b.mu.Unlock()
		slog.Debug("recording already requested", "job", id)
		return
	}
	now := time.Now().UTC()
	job := Job{
		ID:          id,
		MessageID:   m.ID,
		Recorder:    recorder,
		URL:         url,
		RequestedAt: now,
		Deadline:    now.Add(time.Duration(b.joinTimeoutS(recorder)+b.cfg.MaxDurationS)*time.Second + watchdogSlack),
		State:       jobStarting,
	}
	if m.Type == "private" {
		job.DMUserID = m.SenderID
	} else {
		job.Stream, job.Topic = string(m.DisplayRecipient), m.Subject
	}
	err = job.save(b.cfg.DataDir)
	b.mu.Unlock()
	if err != nil {
		slog.Error("saving the job failed", "job", id, "err", err)
		return
	}
	// Log the room name or meeting code only — a full URL may carry a token.
	slog.Info("recording requested", "job", id, "recorder", recorder, "room", path.Base(url))
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		b.launch(ctx, job)
	}()
}

func (b *Bot) joinTimeoutS(recorder string) int {
	if recorder == recorderMeet {
		return b.cfg.MeetJoinTimeoutS
	}
	return b.cfg.JoinTimeoutS
}

func (b *Bot) recorderURL(recorder string) string {
	if recorder == recorderMeet {
		return b.cfg.MeetRecorderURL
	}
	return b.cfg.JitsiRecorderURL
}

// launch shows 🔴, sends POST /recordings and records the outcome. A refusal
// or an unreachable recorder ends in the recorder_failed note and no 🔴.
func (b *Bot) launch(ctx context.Context, job Job) {
	if err := b.z.AddReaction(ctx, job.MessageID, recordingEmoji); err != nil {
		slog.Error("adding the recording indicator failed", "job", job.ID, "err", err)
	}
	err := requestRecording(ctx, b.recorderURL(job.Recorder), b.cfg.RecorderSecret, recordingRequest{
		ID:           job.ID,
		URL:          job.URL,
		CallbackURL:  b.cfg.PublicURL + "/events",
		Meta:         map[string]any{"message_id": job.MessageID},
		DisplayName:  b.cfg.BotDisplayName,
		JoinTimeoutS: b.joinTimeoutS(job.Recorder),
		MaxDurationS: b.cfg.MaxDurationS,
		EmptyGraceS:  b.cfg.EmptyGraceS,
	})
	if ctx.Err() != nil {
		// Shutdown mid-request leaves the job "starting"; the watchdog asks the
		// recorder about it after the deadline.
		return
	}

	// Under the lock, so neither a click nor a recorder event can interleave
	// with the cleanup below.
	b.mu.Lock()
	defer b.mu.Unlock()
	cur, lerr := loadJob(b.cfg.DataDir, job.ID)
	if lerr != nil || cur.State != jobStarting {
		// A recorder event got here first: the recorder has the job, and the
		// event owns the state and what the user is told.
		return
	}
	if err == nil {
		cur.State = jobRunning
		slog.Info("recording accepted", "job", job.ID)
	} else {
		slog.Error("recording request failed", "job", job.ID, "err", err)
		if err := b.reply(ctx, job, noteRecorderFailed); err != nil {
			slog.Error("posting the failure note failed", "job", job.ID, "err", err)
		}
		if err := b.z.RemoveReaction(ctx, job.MessageID, recordingEmoji); err != nil {
			slog.Error("removing the recording indicator failed", "job", job.ID, "err", err)
		}
		cur.State, cur.Error = jobFailed, "recorder_failed"
	}
	if err := cur.save(b.cfg.DataDir); err != nil {
		slog.Error("saving the job failed", "job", job.ID, "err", err)
	}
}

// reply posts into the job's stream/topic, or its DM.
func (b *Bot) reply(ctx context.Context, job Job, content string) error {
	if job.DMUserID != 0 {
		return b.z.SendDM(ctx, job.DMUserID, content)
	}
	return b.z.SendMessage(ctx, job.Stream, job.Topic, content)
}

// pause sleeps for d, or returns early when ctx ends.
func pause(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
