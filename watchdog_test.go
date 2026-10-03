package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// statusRecorder answers GET /recordings/{id} with a fixed status and body and
// counts the signed requests.
type statusRecorder struct {
	*httptest.Server
	mu     sync.Mutex
	status int
	body   string
	gets   int
}

func newStatusRecorder(t *testing.T, status int, body string) *statusRecorder {
	s := &statusRecorder{status: status, body: body}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/recordings/100" {
			t.Errorf("recorder got %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("x-recorder-signature") != sign(nil, testSecret) {
			t.Errorf("bad signature %q", r.Header.Get("x-recorder-signature"))
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.gets++
		w.WriteHeader(s.status)
		io.WriteString(w, s.body)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *statusRecorder) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets
}

// overdue seeds job 100 past its deadline with the jitsi recorder at url.
func (f *botFixture) overdue(t *testing.T, url string) time.Time {
	t.Helper()
	f.seedJob(t, 100, recorderJitsi, false)
	j := f.job(t, "100")
	j.Deadline = time.Now().Add(-time.Minute)
	if err := j.save(f.dataDir); err != nil {
		t.Fatal(err)
	}
	f.bot.cfg.JitsiRecorderURL = url
	return time.Now()
}

func TestWatchdogStillRunningExtendsTheDeadline(t *testing.T) {
	f := newBotFixture(t)
	r := newStatusRecorder(t, http.StatusOK, `{"id":"100","state":"recording"}`)
	now := f.overdue(t, r.URL)
	f.bot.checkJobs(context.Background(), now)
	j := f.job(t, "100")
	if r.count() != 1 || j.State != jobRunning || !j.Deadline.Equal(now.Add(watchdogSlack)) {
		t.Errorf("gets = %d, job = %+v", r.count(), j)
	}
	wantCalls(t, f.zulipCalls())
	// Not overdue any more: the next pass leaves it alone.
	f.bot.checkJobs(context.Background(), now.Add(time.Minute))
	if r.count() != 1 {
		t.Errorf("gets = %d", r.count())
	}
}

func TestWatchdogRecoversALostFinishedEvent(t *testing.T) {
	f := newBotFixture(t)
	r := newStatusRecorder(t, http.StatusOK, `{"id":"100","state":"finished","started_at":"2026-01-01T10:00:00Z",`+
		`"ended_at":"2026-01-01T10:30:34Z","duration_s":1834.2,"participants":["Alice"],"artifacts":`+testAudio+`}`)
	f.bot.checkJobs(context.Background(), f.overdue(t, r.URL))
	wantCalls(t, f.zulipCalls(), unindicate)
	j := f.job(t, "100")
	if j.State != jobFinished || j.LastEvent != evFinished || j.DurationS != 1834.2 || len(j.Artifacts) != 1 {
		t.Errorf("job = %+v", j)
	}
}

func TestWatchdogRecoversALostFailedEvent(t *testing.T) {
	f := newBotFixture(t)
	r := newStatusRecorder(t, http.StatusOK, `{"id":"100","state":"failed","error":"not_admitted"}`)
	f.bot.checkJobs(context.Background(), f.overdue(t, r.URL))
	wantCalls(t, f.zulipCalls(), unindicate,
		"POST /api/v1/messages NoteTaker was not admitted to the call (or nobody joined) — nothing recorded.")
	if j := f.job(t, "100"); j.State != jobFailed || j.Error != "not_admitted" {
		t.Errorf("job = %+v", j)
	}
}

const noteLost = "POST /api/v1/messages Recording was lost (recorder unavailable) — no transcript."

func TestWatchdogThree404sMarkTheJobLost(t *testing.T) {
	f := newBotFixture(t)
	r := newStatusRecorder(t, http.StatusNotFound, "")
	now := f.overdue(t, r.URL)
	for i := 1; i < watchdogMaxMisses; i++ {
		f.bot.checkJobs(context.Background(), now)
		if j := f.job(t, "100"); j.State != jobRunning || j.WatchdogMisses != i {
			t.Fatalf("after %d checks job = %+v", i, j)
		}
	}
	wantCalls(t, f.zulipCalls())
	f.bot.checkJobs(context.Background(), now)
	wantCalls(t, f.zulipCalls(), unindicate, noteLost)
	if j := f.job(t, "100"); j.State != jobFailed || j.Error != "lost" {
		t.Errorf("job = %+v", j)
	}
	// Settled: no more checks, no second note.
	f.bot.checkJobs(context.Background(), now)
	if r.count() != watchdogMaxMisses || len(f.zulipCalls()) != 2 {
		t.Errorf("gets = %d, calls = %q", r.count(), f.zulipCalls())
	}
}

func TestWatchdogUnreachableThreeTimesMarksTheJobLost(t *testing.T) {
	f := newBotFixture(t)
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	now := f.overdue(t, gone.URL)
	for range watchdogMaxMisses {
		f.bot.checkJobs(context.Background(), now)
	}
	wantCalls(t, f.zulipCalls(), unindicate, noteLost)
	if j := f.job(t, "100"); j.State != jobFailed || j.Error != "lost" {
		t.Errorf("job = %+v", j)
	}
}

// A recording found again resets the miss count.
func TestWatchdogMissesMustBeConsecutive(t *testing.T) {
	f := newBotFixture(t)
	r := newStatusRecorder(t, http.StatusNotFound, "")
	now := f.overdue(t, r.URL)
	f.bot.checkJobs(context.Background(), now)
	f.bot.checkJobs(context.Background(), now)
	r.mu.Lock()
	r.status, r.body = http.StatusOK, `{"state":"recording"}`
	r.mu.Unlock()
	f.bot.checkJobs(context.Background(), now)
	if j := f.job(t, "100"); j.State != jobRunning || j.WatchdogMisses != 0 {
		t.Errorf("job = %+v", j)
	}
}

// After a restart the jobs on disk stay as they were: a running job before
// its deadline is not asked about, ended jobs never are, and an overdue start
// cut short by the shutdown is.
func TestWatchdogAfterRestartKeepsJobs(t *testing.T) {
	f := newBotFixture(t)
	r := newStatusRecorder(t, http.StatusOK, `{"state":"joining"}`)
	f.bot.cfg.JitsiRecorderURL = r.URL
	now := time.Now()
	for id, st := range map[string]string{"101": jobRunning, "102": jobFinished, "103": jobFailed} {
		j := Job{ID: id, MessageID: 1, Recorder: recorderJitsi, State: st, Deadline: now.Add(-time.Minute)}
		if st == jobRunning {
			j.Deadline = now.Add(time.Hour)
		}
		if err := j.save(f.dataDir); err != nil {
			t.Fatal(err)
		}
	}
	restarted := newBot(f.bot.cfg, f.bot.z)
	restarted.checkJobs(context.Background(), now)
	if r.count() != 0 {
		t.Errorf("gets = %d", r.count())
	}
	if j := f.job(t, "101"); j.State != jobRunning || !j.Deadline.Equal(now.Add(time.Hour)) {
		t.Errorf("job = %+v", j)
	}

	j := Job{ID: "100", MessageID: 100, Recorder: recorderJitsi, State: jobStarting, Deadline: now.Add(-time.Minute)}
	if err := j.save(f.dataDir); err != nil {
		t.Fatal(err)
	}
	restarted.checkJobs(context.Background(), now)
	if r.count() != 1 {
		t.Errorf("gets = %d", r.count())
	}
	if j := f.job(t, "100"); j.State != jobStarting || !j.Deadline.Equal(now.Add(watchdogSlack)) {
		t.Errorf("job = %+v", j)
	}
	wantCalls(t, f.zulipCalls())
}

// A Zulip outage while settling leaves the job watched; the next pass retries.
func TestWatchdogRetriesAfterAZulipFailure(t *testing.T) {
	f := newBotFixture(t)
	r := newStatusRecorder(t, http.StatusOK, `{"state":"failed","error":"recorder_failed"}`)
	now := f.overdue(t, r.URL)
	f.refuse.Store("503 POST /api/v1/messages")
	f.bot.checkJobs(context.Background(), now)
	if j := f.job(t, "100"); j.State != jobRunning {
		t.Fatalf("job = %+v", j)
	}
	f.refuse.Store("")
	f.bot.checkJobs(context.Background(), now)
	if j := f.job(t, "100"); j.State != jobFailed || j.Error != "recorder_failed" {
		t.Errorf("job = %+v", j)
	}
}
