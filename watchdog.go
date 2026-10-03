package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// watchdogInterval is how often the watchdog looks for overdue jobs.
const watchdogInterval = time.Minute

// watchdogMaxMisses is how many checks in a row may find the job missing (404)
// or the recorder unreachable before the job is reported lost.
const watchdogMaxMisses = 3

// recordingRecord is the part of the recorder's job record (§3.3, §3.6) the
// watchdog reads; the field names match the events'.
type recordingRecord struct {
	State        string     `json:"state"`
	Error        string     `json:"error"`
	StartedAt    string     `json:"started_at"`
	EndedAt      string     `json:"ended_at"`
	DurationS    float64    `json:"duration_s"`
	Participants []string   `json:"participants"`
	Artifacts    []Artifact `json:"artifacts"`
}

// errNoRecording is a 404 from GET /recordings/{id}.
var errNoRecording = errors.New("recorder does not know the job")

// getRecording is GET /recordings/{id}, signed over the empty body. One
// attempt: the watchdog's next check is the retry.
func getRecording(ctx context.Context, base, secret, id string) (recordingRecord, error) {
	var rec recordingRecord
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/recordings/"+id, nil)
	if err != nil {
		return rec, err
	}
	req.Header.Set("x-recorder-signature", sign(nil, secret))
	resp, err := recorderHTTP.Do(req)
	if err != nil {
		return rec, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return rec, errNoRecording
	case resp.StatusCode != http.StatusOK:
		return rec, fmt.Errorf("recorder answered http %d", resp.StatusCode)
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, maxEventBody)).Decode(&rec)
	return rec, err
}

// watchdog checks overdue jobs every watchdogInterval until ctx ends. Jobs
// survive a restart as they are: the first pass right at startup picks up
// whatever was running before.
func (b *Bot) watchdog(ctx context.Context) {
	t := time.NewTicker(watchdogInterval)
	defer t.Stop()
	for {
		b.checkJobs(ctx, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// checkJobs asks the recorder about every unended job past its deadline.
//
// ponytail: scans every job.json each minute; an index of open jobs if the
// directory ever grows large.
func (b *Bot) checkJobs(ctx context.Context, now time.Time) {
	dirs, err := os.ReadDir(filepath.Join(b.cfg.DataDir, "jobs"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Error("listing jobs failed", "err", err)
		return
	}
	for _, d := range dirs {
		if ctx.Err() != nil {
			return
		}
		job, err := loadJob(b.cfg.DataDir, d.Name())
		if err != nil {
			slog.Error("reading the job failed", "job", d.Name(), "err", err)
			continue
		}
		if watched(job) && now.After(job.Deadline) {
			b.checkJob(ctx, job, now)
		}
	}
}

// watched is a job still waiting for a terminal event: running, or a start
// cut short by a shutdown. A start that failed locally was already reported.
func watched(job Job) bool { return job.State == jobStarting || job.State == jobRunning }

func (b *Bot) checkJob(ctx context.Context, job Job, now time.Time) {
	rec, gerr := getRecording(ctx, b.recorderURL(job.Recorder), b.cfg.RecorderSecret, job.ID)
	if ctx.Err() != nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	// Re-read under the lock: an event may have ended the job meanwhile.
	cur, err := loadJob(b.cfg.DataDir, job.ID)
	if err != nil || !watched(cur) {
		return
	}
	var ev recorderEvent
	switch {
	case gerr == nil && rec.State == "finished":
		ev = recorderEvent{Event: evFinished, ID: cur.ID, StartedAt: rec.StartedAt, EndedAt: rec.EndedAt,
			DurationS: rec.DurationS, Participants: rec.Participants, Artifacts: rec.Artifacts}
	case gerr == nil && rec.State == "failed":
		ev = recorderEvent{Event: evFailed, ID: cur.ID, Error: rec.Error, Artifacts: rec.Artifacts}
	case gerr == nil: // joining, recording: still busy
		// ponytail: now+slack rather than deadline+slack, so a bot that was down
		// for hours does not ask every minute.
		cur.Deadline, cur.WatchdogMisses = now.Add(watchdogSlack), 0
		slog.Info("overdue job still running", "job", cur.ID, "recorder_state", rec.State)
		if err := cur.save(b.cfg.DataDir); err != nil {
			slog.Error("saving the job failed", "job", cur.ID, "err", err)
		}
		return
	default:
		cur.WatchdogMisses++
		slog.Warn("overdue job not found on the recorder", "job", cur.ID, "misses", cur.WatchdogMisses, "err", gerr)
		if err := cur.save(b.cfg.DataDir); err != nil {
			slog.Error("saving the job failed", "job", cur.ID, "err", err)
			return
		}
		if cur.WatchdogMisses < watchdogMaxMisses {
			return
		}
		ev = recorderEvent{Event: evFailed, ID: cur.ID, Error: "lost"}
	}
	// The event was lost (or the recorder is gone): handle the outcome as if it
	// had arrived. A failure leaves the job watched and the next pass retries.
	slog.Info("watchdog settles an overdue job", "job", cur.ID, "event", ev.Event, "error", ev.Error)
	if err := b.applyEventLocked(ctx, ev); err != nil {
		slog.Error("watchdog could not settle the job", "job", cur.ID, "err", err)
	}
}
