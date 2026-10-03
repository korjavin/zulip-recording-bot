package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const testWebhookSecret = "test-webhook-secret"

// fakeTranscriber answers with the next status in statuses (the last repeats)
// and keeps every request.
type fakeTranscriber struct {
	*httptest.Server
	statuses []int

	mu     sync.Mutex
	bodies [][]byte
	heads  []http.Header
}

func newFakeTranscriber(t *testing.T, f *botFixture, statuses ...int) *fakeTranscriber {
	old := handOffRetryDelays
	handOffRetryDelays = []time.Duration{0, 0, 0, 0, 0}
	t.Cleanup(func() { handOffRetryDelays = old })
	tr := &fakeTranscriber{statuses: statuses}
	tr.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		tr.mu.Lock()
		tr.bodies = append(tr.bodies, body)
		tr.heads = append(tr.heads, r.Header.Clone())
		st := tr.statuses[min(len(tr.bodies), len(tr.statuses))-1]
		tr.mu.Unlock()
		w.WriteHeader(st)
	}))
	t.Cleanup(tr.Close)
	f.bot.cfg.WebhookURL, f.bot.cfg.WebhookSecret = tr.URL, testWebhookSecret
	return tr
}

func (tr *fakeTranscriber) count() int {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return len(tr.bodies)
}

func wantJSON(t *testing.T, got []byte, want string) {
	t.Helper()
	var w bytes.Buffer
	if err := json.Compact(&w, []byte(want)); err != nil {
		t.Fatal(err)
	}
	if string(got) != w.String() {
		t.Errorf("body:\n%s\nwant:\n%s", got, w.String())
	}
}

func TestHandOffBodyJitsiWithTracksAndSpeakers(t *testing.T) {
	f := newBotFixture(t)
	f.seedJob(t, 100, recorderJitsi, false)
	wantStatus(t, f.post(event(evFinished, "100", `,"started_at":"2026-01-01T10:00:00Z","ended_at":"2026-01-01T10:30:34Z",`+
		`"duration_s":1834.2,"reason":"empty_room","participants":["Alice","Bob"],"artifacts":[`+
		`{"kind":"audio","path":"/data/jitsi/100/audio.webm","format":"webm"},`+
		`{"kind":"track","path":"/data/jitsi/100/tracks/p1.webm","participant_id":"p1","name":"Alice","offset_s":0,"ended_s":1834.2},`+
		`{"kind":"track","path":"/data/jitsi/100/tracks/p2.webm","participant_id":"p2","name":"Bob","offset_s":12.5,"ended_s":900},`+
		`{"kind":"speakers","path":"/data/jitsi/100/tracks/speakers.jsonl"},`+
		`{"kind":"future","path":"/data/jitsi/100/x"}]`)), http.StatusOK)
	j := f.job(t, "100")
	if j.Webhook != handOffPending {
		t.Errorf("webhook = %q", j.Webhook)
	}
	got, _ := json.Marshal(f.bot.handOffBody(j))
	wantJSON(t, got, `{
		"event": "recording.finished", "id": "100", "message_id": 100,
		"stream": "`+testStream+`", "topic": "`+testTopic+`",
		"jitsi_url": "`+testRoomURL+`",
		"audio_path": "/data/jitsi/100/audio.webm",
		"duration_s": 1834.2, "started_at": "2026-01-01T10:00:00Z", "ended_at": "2026-01-01T10:30:34Z",
		"participants": ["Alice", "Bob"],
		"callback_url": "http://bot.example.com:8080/notify",
		"tracks": [
			{"id": "p1", "name": "Alice", "path": "/data/jitsi/100/tracks/p1.webm", "offset_s": 0, "ended_s": 1834.2},
			{"id": "p2", "name": "Bob", "path": "/data/jitsi/100/tracks/p2.webm", "offset_s": 12.5, "ended_s": 900}
		]
	}`)
}

func TestHandOffBodyMeetDMWithCaptions(t *testing.T) {
	f := newBotFixture(t)
	f.seedJob(t, 300, recorderMeet, true)
	wantStatus(t, f.post(event(evFinished, "300", `,"started_at":"2026-01-01T10:00:00Z","ended_at":"2026-01-01T10:20:00Z",`+
		`"duration_s":1200,"reason":"ended","artifacts":[`+
		`{"kind":"audio","path":"/data/meet/300/audio.wav","format":"wav"},`+
		`{"kind":"captions","path":"/data/meet/300/captions.jsonl"}]`)), http.StatusOK)
	got, _ := json.Marshal(f.bot.handOffBody(f.job(t, "300")))
	wantJSON(t, got, `{
		"event": "recording.finished", "id": "300", "message_id": 300,
		"stream": "", "topic": "", "dm_user_id": 42,
		"jitsi_url": "`+testMeet+`", "source": "meet",
		"audio_path": "/data/meet/300/audio.wav",
		"duration_s": 1200, "started_at": "2026-01-01T10:00:00Z", "ended_at": "2026-01-01T10:20:00Z",
		"participants": [],
		"callback_url": "http://bot.example.com:8080/notify",
		"speaker_hints_path": "/data/meet/300/captions.jsonl"
	}`)
}

// A finished recording wakes the loop; a 500 is retried, and the signed body
// lands once.
func TestHandOffRetriesAfter500(t *testing.T) {
	f := newBotFixture(t)
	tr := newFakeTranscriber(t, f, http.StatusInternalServerError, http.StatusAccepted)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.bot.handOffLoop(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	f.seedJob(t, 100, recorderJitsi, false)
	wantStatus(t, f.post(event(evFinished, "100", `,"duration_s":60,"artifacts":`+testAudio)), http.StatusOK)
	for deadline := time.Now().Add(5 * time.Second); f.job(t, "100").Webhook != handOffSent; {
		if time.Now().After(deadline) {
			t.Fatalf("not sent; %d attempts", tr.count())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if tr.count() != 2 {
		t.Fatalf("attempts = %d", tr.count())
	}
	body, h := tr.bodies[1], tr.heads[1]
	if h.Get("x-jitsi-capture-event") != evFinished || h.Get("x-jitsi-capture-signature") != sign(body, testWebhookSecret) {
		t.Errorf("headers = %v", h)
	}
	if !strings.Contains(string(body), `"audio_path":"/data/x/audio.webm"`) {
		t.Errorf("body = %s", body)
	}
}

// After its retries a failing hand-off stays pending for the next sweep; a
// short recording is never pending at all.
func TestHandOffStaysPendingAndShortIsSkipped(t *testing.T) {
	f := newBotFixture(t)
	tr := newFakeTranscriber(t, f, http.StatusBadGateway)
	f.seedJob(t, 100, recorderJitsi, false)
	f.seedJob(t, 200, recorderJitsi, false)
	wantStatus(t, f.post(event(evFinished, "100", `,"duration_s":60,"artifacts":`+testAudio)), http.StatusOK)
	wantStatus(t, f.post(event(evFinished, "200", `,"duration_s":5,"artifacts":`+testAudio)), http.StatusOK)
	f.bot.sweepHandOffs(context.Background())
	if n := tr.count(); n != len(handOffRetryDelays)+1 {
		t.Errorf("attempts = %d", n)
	}
	if j := f.job(t, "100"); j.Webhook != handOffPending {
		t.Errorf("webhook = %q", j.Webhook)
	}
	if j := f.job(t, "200"); j.Webhook != "" {
		t.Errorf("short recording webhook = %q", j.Webhook)
	}
	tr.statuses = []int{http.StatusOK} // the sweep at startup / next hour
	f.bot.sweepHandOffs(context.Background())
	if j := f.job(t, "100"); j.Webhook != handOffSent {
		t.Errorf("webhook = %q", j.Webhook)
	}
}

func (f *botFixture) notify(body, sig string) int {
	req := httptest.NewRequest(http.MethodPost, "/notify", strings.NewReader(body))
	req.Header.Set("x-jitsi-capture-signature", sig)
	rec := httptest.NewRecorder()
	handler(f.bot).ServeHTTP(rec, req)
	return rec.Code
}

func (f *botFixture) signedNotify(body string) int {
	return f.notify(body, sign([]byte(body), testWebhookSecret))
}

const readyNote = `{"id":"100","content":"Transcript ready: https://docs.example.com/doc/x"}`

func TestNotifyPostsVerbatim(t *testing.T) {
	for _, viaDM := range []bool{false, true} {
		f := newBotFixture(t)
		f.bot.cfg.WebhookSecret = testWebhookSecret
		f.seedJob(t, 100, recorderJitsi, viaDM)
		wantStatus(t, f.signedNotify(readyNote), http.StatusOK)
		wantCalls(t, f.zulipCalls(), "POST /api/v1/messages Transcript ready: https://docs.example.com/doc/x")
		form := f.zulip.requests()[0].form
		if viaDM && form.Get("to") != "[42]" || !viaDM && (form.Get("to") != `"`+testStream+`"` || form.Get("topic") != testTopic) {
			t.Errorf("dm=%v: posted to %v", viaDM, form)
		}
	}
}

func TestNotifyRejections(t *testing.T) {
	f := newBotFixture(t)
	f.bot.cfg.WebhookSecret = testWebhookSecret
	f.seedJob(t, 100, recorderJitsi, false)
	wantStatus(t, f.notify(readyNote, sign([]byte(readyNote), "wrong")), http.StatusUnauthorized)
	wantStatus(t, f.notify(readyNote, ""), http.StatusUnauthorized)
	wantStatus(t, f.signedNotify(`{"id":"100"}`), http.StatusBadRequest)
	wantStatus(t, f.signedNotify(`not json`), http.StatusBadRequest)
	wantStatus(t, f.signedNotify(`{"id":"999","content":"x"}`), http.StatusNotFound)
	wantStatus(t, f.signedNotify(`{"id":"../jobs/100","content":"x"}`), http.StatusNotFound)
	wantCalls(t, f.zulipCalls())

	// No secret configured: nothing is trusted.
	f.bot.cfg.WebhookSecret = ""
	wantStatus(t, f.notify(readyNote, sign([]byte(readyNote), "")), http.StatusUnauthorized)
}

func TestNotifyZulipRefusalIs502(t *testing.T) {
	for _, refuse := range []string{"400 POST /api/v1/messages", "503 POST /api/v1/messages"} {
		f := newBotFixture(t)
		f.bot.cfg.WebhookSecret = testWebhookSecret
		f.seedJob(t, 100, recorderJitsi, false)
		f.refuse.Store(refuse)
		wantStatus(t, f.signedNotify(readyNote), http.StatusBadGateway)
	}
}

// A job the transcriber refuses (4xx, or a 5xx after its retries) is not
// retried at once and does not hold up the jobs after it.
func TestRefusedHandOffDoesNotStarveOthers(t *testing.T) {
	f := newBotFixture(t)
	tr := newFakeTranscriber(t, f, http.StatusBadRequest, http.StatusOK)
	for _, id := range []string{"100", "200"} {
		j := Job{ID: id, State: jobFinished, Webhook: handOffPending, DurationS: 60}
		if err := j.save(f.dataDir); err != nil {
			t.Fatal(err)
		}
	}
	f.bot.sweepHandOffs(context.Background())
	if tr.count() != 2 || f.job(t, "100").Webhook != handOffPending || f.job(t, "200").Webhook != handOffSent {
		t.Errorf("attempts = %d; jobs %q %q", tr.count(), f.job(t, "100").Webhook, f.job(t, "200").Webhook)
	}
}

func TestHandOffErrorHidesTheWebhookURL(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close() // nothing listens there now
	err := postHandOff(context.Background(), srv.URL+"/hook?token=s3cret", "k", []byte("{}"))
	if !errors.Is(err, errTranscriberDown) || strings.Contains(err.Error(), "s3cret") {
		t.Errorf("err = %v", err)
	}
	err = postHandOff(context.Background(), "http://transcriber.example.com/hook%zz?token=s3cret", "k", []byte("{}"))
	if err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Errorf("err = %v", err)
	}
}
