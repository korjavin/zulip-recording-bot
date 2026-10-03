package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Job is the bot's record of one recording, stored at
// DATA_DIR/jobs/<id>/job.json. ID is the Zulip message id of the call link
// message (stream) or the request DM.
type Job struct {
	ID        string `json:"id"`
	MessageID int64  `json:"message_id"`
	Stream    string `json:"stream,omitempty"`
	Topic     string `json:"topic,omitempty"`
	DMUserID  int64  `json:"dm_user_id,omitempty"` // a DM-started job; replies go back to this user

	Recorder    string     `json:"recorder"` // recorderJitsi | recorderMeet
	URL         string     `json:"url"`      // room URL only — never a token or password
	RequestedAt time.Time  `json:"requested_at"`
	Deadline    time.Time  `json:"deadline"` // when the watchdog stops waiting for a terminal event
	State       string     `json:"state"`
	Error       string     `json:"error,omitempty"`
	LastEvent   string     `json:"last_event,omitempty"`
	Artifacts   []Artifact `json:"artifacts,omitempty"`
}

// Artifact is one file a recorder reported (docs/architecture.md §3.5).
type Artifact struct {
	Kind          string  `json:"kind"`
	Path          string  `json:"path"`
	Format        string  `json:"format,omitempty"`
	ParticipantID string  `json:"participant_id,omitempty"`
	Name          string  `json:"name,omitempty"`
	OffsetS       float64 `json:"offset_s,omitempty"`
	EndedS        float64 `json:"ended_s,omitempty"`
}

const (
	recorderJitsi = "jitsi"
	recorderMeet  = "meet"
)

// Job.State values. A job is "starting" while the bot is still asking the
// recorder, "running" once the recorder accepted it.
const (
	jobStarting = "starting"
	jobRunning  = "running"
	jobFailed   = "failed"
)

func jobDir(dataDir, id string) string { return filepath.Join(dataDir, "jobs", id) }

// save writes job.json atomically: a temp file in the same directory, then a
// rename, so a crash mid-write never leaves a half-written record.
func (j *Job) save(dataDir string) error {
	dir := jobDir(dataDir, j.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "job-*.json.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once the rename succeeded
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, "job.json"))
}

// loadJob reads one job; a missing one wraps os.ErrNotExist.
func loadJob(dataDir, id string) (Job, error) {
	var j Job
	b, err := os.ReadFile(filepath.Join(jobDir(dataDir, id), "job.json"))
	if err != nil {
		return j, err
	}
	return j, json.Unmarshal(b, &j)
}
