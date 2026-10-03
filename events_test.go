package main

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

const testAudio = `[{"kind":"audio","path":"/data/x/audio.webm","format":"webm"}]`

// seedJob stores a running job as if a click (stream) or a DM had started it.
func (f *botFixture) seedJob(t *testing.T, id int64, recorder string, viaDM bool) {
	t.Helper()
	j := Job{ID: strconv.FormatInt(id, 10), MessageID: id, Recorder: recorder, URL: testRoomURL, State: jobRunning}
	if recorder == recorderMeet {
		j.URL = testMeet
	}
	if viaDM {
		j.DMUserID = testUserID
	} else {
		j.Stream, j.Topic = testStream, testTopic
	}
	if err := j.save(f.dataDir); err != nil {
		t.Fatal(err)
	}
}

// post delivers a signed event body and returns the status code.
func (f *botFixture) post(body string) int {
	return f.postSigned(body, sign([]byte(body), testSecret))
}

func (f *botFixture) postSigned(body, sig string) int {
	req := httptest.NewRequest(http.MethodPost, "/events", strings.NewReader(body))
	req.Header.Set("x-recorder-signature", sig)
	rec := httptest.NewRecorder()
	handler(f.bot).ServeHTTP(rec, req)
	return rec.Code
}

func event(name, id, extra string) string {
	return `{"event":"` + name + `","id":"` + id + `","source":"jitsi","url":"x","meta":{},"at":"2026-01-01T10:30:34Z"` + extra + `}`
}

func wantStatus(t *testing.T, got, want int) {
	t.Helper()
	if got != want {
		t.Errorf("status = %d; want %d", got, want)
	}
}

const unindicate = "DELETE /api/v1/messages/100/reactions " + recordingEmoji

func TestEventBadSignatureIs401(t *testing.T) {
	f := newBotFixture(t)
	f.seedJob(t, 100, recorderJitsi, false)
	body := event(evFailed, "100", `,"error":"recorder_failed"`)
	wantStatus(t, f.postSigned(body, sign([]byte(body), "wrong")), http.StatusUnauthorized)
	wantStatus(t, f.postSigned(body, ""), http.StatusUnauthorized)
	wantCalls(t, f.zulipCalls())
	if j := f.job(t, "100"); j.State != jobRunning || len(j.Events) != 0 {
		t.Errorf("job = %+v", j)
	}
}

func TestEventUnknownOrBadJob(t *testing.T) {
	f := newBotFixture(t)
	wantStatus(t, f.post(event(evStarted, "999", "")), http.StatusNotFound)
	wantStatus(t, f.post(event(evStarted, "../../etc", "")), http.StatusNotFound)
	wantStatus(t, f.post(`{"id":"100"}`), http.StatusBadRequest)
	wantStatus(t, f.post(`not json`), http.StatusBadRequest)
	wantCalls(t, f.zulipCalls())
}

func TestWaitingAdmissionMeetAsksToBeAdmitted(t *testing.T) {
	f := newBotFixture(t)
	f.seedJob(t, 300, recorderMeet, true)
	wantStatus(t, f.post(event(evWaitingAdmission, "300", "")), http.StatusOK)
	wantCalls(t, f.zulipCalls(), "POST /api/v1/messages Asking to join abc-defg-hij as a guest — admit NoteTaker from the lobby.")
	if to := f.zulip.requests()[0].form.Get("to"); to != "[42]" {
		t.Errorf("DM went to %q", to)
	}
	if j := f.job(t, "300"); j.State != jobRunning || j.LastEvent != evWaitingAdmission {
		t.Errorf("job = %+v", j)
	}
}

func TestWaitingAdmissionAndStartedJitsiAreSilent(t *testing.T) {
	f := newBotFixture(t)
	f.seedJob(t, 100, recorderJitsi, false)
	wantStatus(t, f.post(event(evWaitingAdmission, "100", "")), http.StatusOK)
	wantStatus(t, f.post(event(evStarted, "100", "")), http.StatusOK)
	wantCalls(t, f.zulipCalls())
	if j := f.job(t, "100"); j.State != jobRunning || j.LastEvent != evStarted {
		t.Errorf("job = %+v", j)
	}
}

func TestFinishedIsStoredAndUnindicated(t *testing.T) {
	f := newBotFixture(t)
	f.seedJob(t, 100, recorderJitsi, false)
	body := event(evFinished, "100", `,"started_at":"2026-01-01T10:00:00Z","ended_at":"2026-01-01T10:30:34Z",`+
		`"duration_s":1834.2,"reason":"empty_room","participants":["Alice","Bob"],"artifacts":`+testAudio)
	wantStatus(t, f.post(body), http.StatusOK)
	wantCalls(t, f.zulipCalls(), unindicate)
	j := f.job(t, "100")
	if j.State != jobFinished || j.LastEvent != evFinished || j.DurationS != 1834.2 ||
		j.StartedAt != "2026-01-01T10:00:00Z" || len(j.Participants) != 2 ||
		len(j.Artifacts) != 1 || j.Artifacts[0].Path != "/data/x/audio.webm" {
		t.Errorf("job = %+v", j)
	}
}

func TestFinishedMinRecordingBoundary(t *testing.T) {
	for _, tc := range []struct {
		dur   string
		short bool
	}{{"14.9", true}, {"15", false}, {"0", true}} {
		f := newBotFixture(t)
		f.seedJob(t, 100, recorderJitsi, false)
		wantStatus(t, f.post(event(evFinished, "100", `,"duration_s":`+tc.dur+`,"artifacts":`+testAudio)), http.StatusOK)
		if tc.short {
			wantCalls(t, f.zulipCalls(), unindicate, "POST /api/v1/messages Recording too short (under 15 s) — nothing to transcribe.")
		} else {
			wantCalls(t, f.zulipCalls(), unindicate)
		}
	}
}

func TestFailedNotes(t *testing.T) {
	for _, tc := range []struct{ err, artifacts, note string }{
		{"not_admitted", "", "NoteTaker was not admitted to the call (or nobody joined) — nothing recorded."},
		{"recorder_failed", "", "Recording failed (recorder error) — nothing recorded."},
		{"interrupted", "", "Recording was interrupted by a service restart — no transcript."},
		{"something_new", "", "Recording failed (recorder error) — nothing recorded."},
		{"interrupted", testAudio, "Recording was interrupted by a service restart — a partial recording was kept (job 100), no transcript."},
		{"recorder_failed", testAudio, "Recording failed (recorder error) — a partial recording was kept (job 100), no transcript."},
	} {
		f := newBotFixture(t)
		f.seedJob(t, 100, recorderJitsi, false)
		extra := `,"error":"` + tc.err + `"`
		if tc.artifacts != "" {
			extra += `,"artifacts":` + tc.artifacts
		}
		wantStatus(t, f.post(event(evFailed, "100", extra)), http.StatusOK)
		wantCalls(t, f.zulipCalls(), unindicate, "POST /api/v1/messages "+tc.note)
		if form := f.zulip.requests()[1].form; form.Get("to") != `"`+testStream+`"` || form.Get("topic") != testTopic {
			t.Errorf("note went to %v", form)
		}
		if j := f.job(t, "100"); j.State != jobFailed || j.Error != tc.err || j.LastEvent != evFailed {
			t.Errorf("job = %+v", j)
		}
	}
}

func TestFailedInADMJobGoesToTheDM(t *testing.T) {
	f := newBotFixture(t)
	f.seedJob(t, 100, recorderJitsi, true)
	wantStatus(t, f.post(event(evFailed, "100", `,"error":"not_admitted"`)), http.StatusOK)
	if to := f.zulip.requests()[1].form.Get("to"); to != "[42]" {
		t.Errorf("note went to %q", to)
	}
}

func TestDuplicateAndLateEventsHaveNoSideEffects(t *testing.T) {
	f := newBotFixture(t)
	f.seedJob(t, 100, recorderJitsi, false)
	failed := event(evFailed, "100", `,"error":"recorder_failed"`)
	wantStatus(t, f.post(failed), http.StatusOK)
	wantStatus(t, f.post(failed), http.StatusOK)
	// A best-effort event arriving after the end must not reopen the job.
	wantStatus(t, f.post(event(evStarted, "100", "")), http.StatusOK)
	wantCalls(t, f.zulipCalls(), unindicate, "POST /api/v1/messages "+noteRecorderFailed)
	if j := f.job(t, "100"); j.State != jobFailed || len(j.Events) != 1 {
		t.Errorf("job = %+v", j)
	}
}

func TestZulipFailureIs502AndTheRetryIsHandled(t *testing.T) {
	for _, refuse := range []string{"POST /api/v1/messages", "DELETE /api/v1/messages/100/reactions"} {
		f := newBotFixture(t)
		f.seedJob(t, 100, recorderJitsi, false)
		f.refuse.Store(refuse)
		body := event(evFailed, "100", `,"error":"not_admitted"`)
		wantStatus(t, f.post(body), http.StatusBadGateway)
		if j := f.job(t, "100"); j.State != jobRunning || len(j.Events) != 0 {
			t.Errorf("%s: after a refusal job = %+v", refuse, j)
		}
		f.refuse.Store("")
		wantStatus(t, f.post(body), http.StatusOK)
		if j := f.job(t, "100"); j.State != jobFailed {
			t.Errorf("%s: after the retry job = %+v", refuse, j)
		}
		if calls := f.zulipCalls(); calls[len(calls)-1] != "POST /api/v1/messages "+
			"NoteTaker was not admitted to the call (or nobody joined) — nothing recorded." {
			t.Errorf("%s: calls = %q", refuse, calls)
		}
	}
}

// The recorder reported, so it has the job: a failed POST /recordings answer
// must not tell the user the recording failed.
func TestEventDuringFailedStartKeepsTheRecording(t *testing.T) {
	f := newBotFixture(t, http.StatusUnprocessableEntity)
	f.jitsi.onRequest = func() {
		wantStatus(t, f.post(event(evStarted, "100", "")), http.StatusOK)
	}
	f.click()
	wantCalls(t, f.zulipCalls(), "POST /api/v1/messages/100/reactions "+recordingEmoji)
	if j := f.job(t, "100"); j.State != jobRunning || j.LastEvent != evStarted {
		t.Errorf("job = %+v", j)
	}
}

// A recorder may report before its POST /recordings answer reaches the bot;
// launch must then leave the event's state alone.
func TestEventDuringStartIsNotOverwritten(t *testing.T) {
	f := newBotFixture(t)
	f.jitsi.onRequest = func() {
		wantStatus(t, f.post(event(evFailed, "100", `,"error":"not_admitted"`)), http.StatusOK)
	}
	f.click()
	if j := f.job(t, "100"); j.State != jobFailed || j.Error != "not_admitted" {
		t.Errorf("job = %+v", j)
	}
}
