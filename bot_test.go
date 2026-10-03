package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testSecret = "test-recorder-secret"
	testBotID  = 7
	testUserID = 42
	testMeet   = "https://meet.google.com/abc-defg-hij"
)

// fakeRecorder answers POST /recordings with the next status in statuses (the
// last one repeats) and keeps every body whose signature verified.
type fakeRecorder struct {
	*httptest.Server
	t        *testing.T
	statuses []int

	mu   sync.Mutex
	reqs []recordingRequest
}

func newFakeRecorder(t *testing.T, statuses ...int) *fakeRecorder {
	f := &fakeRecorder{t: t, statuses: statuses}
	f.Server = httptest.NewServer(f)
	t.Cleanup(f.Close)
	return f
}

func (f *fakeRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if r.Method != http.MethodPost || r.URL.Path != "/recordings" {
		f.t.Errorf("recorder got %s %s", r.Method, r.URL.Path)
	}
	if got := r.Header.Get("x-recorder-signature"); got != sign(body, testSecret) {
		f.t.Errorf("bad signature %q", got)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var rr recordingRequest
	if err := json.Unmarshal(body, &rr); err != nil {
		f.t.Errorf("bad body: %v", err)
	}
	f.mu.Lock()
	f.reqs = append(f.reqs, rr)
	st := f.statuses[min(len(f.reqs), len(f.statuses))-1]
	f.mu.Unlock()
	w.WriteHeader(st)
	io.WriteString(w, `{"id":"`+rr.ID+`","state":"joining"}`)
}

func (f *fakeRecorder) requests() []recordingRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordingRequest(nil), f.reqs...)
}

type botFixture struct {
	bot           *Bot
	zulip         *zulipServer
	jitsi, meet   *fakeRecorder
	dataDir       string
	reactedStream Message // what GET /messages/{id} returns
}

func newBotFixture(t *testing.T, statuses ...int) *botFixture {
	t.Helper()
	old := recorderRetryDelays
	recorderRetryDelays = []time.Duration{0, 0, 0}
	t.Cleanup(func() { recorderRetryDelays = old })
	if len(statuses) == 0 {
		statuses = []int{http.StatusAccepted}
	}
	f := &botFixture{
		jitsi:   newFakeRecorder(t, statuses...),
		meet:    newFakeRecorder(t, statuses...),
		dataDir: t.TempDir(),
		reactedStream: Message{ID: 100, Type: "stream", Content: testContent,
			DisplayRecipient: testStream, Subject: testTopic, SenderID: testUserID},
	}
	z, s := newZulipServer(t, func(w http.ResponseWriter, r *http.Request, _ url.Values) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/messages/100" {
			b, _ := json.Marshal(map[string]any{"result": "ok", "message": f.reactedStream})
			ok(w, string(b))
			return
		}
		ok(w, "")
	})
	f.zulip = s
	f.bot = newBot(Config{
		DataDir:          f.dataDir,
		PublicURL:        "http://bot.example.com:8080",
		JitsiBaseURL:     "https://meet.jit.si",
		JitsiRecorderURL: f.jitsi.URL,
		MeetRecorderURL:  f.meet.URL,
		RecorderSecret:   testSecret,
		BotDisplayName:   "NoteTaker",
		JoinTimeoutS:     600,
		MeetJoinTimeoutS: 1200,
		MaxDurationS:     14400,
		EmptyGraceS:      60,
	}, z)
	f.bot.botID = testBotID
	return f
}

// send handles one event and waits for any recorder request it started.
func (f *botFixture) send(ev Event) {
	f.bot.handle(context.Background(), ev)
	f.bot.wg.Wait()
}

func (f *botFixture) click() {
	f.send(Event{Type: "reaction", Op: "add", UserID: testUserID, MessageID: 100, EmojiName: micEmoji})
}

// zulipCalls lists the non-GET Zulip calls as "METHOD path emoji|content".
func (f *botFixture) zulipCalls() []string {
	var out []string
	for _, r := range f.zulip.requests() {
		if r.method == http.MethodGet {
			continue
		}
		out = append(out, r.method+" "+r.path+" "+r.form.Get("emoji_name")+r.form.Get("content"))
	}
	return out
}

func (f *botFixture) job(t *testing.T, id string) Job {
	t.Helper()
	j, err := loadJob(f.dataDir, id)
	if err != nil {
		t.Fatalf("loadJob(%s): %v", id, err)
	}
	return j
}

func wantCalls(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("zulip calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func dm(id int64, content string) Event {
	return Event{Type: "message", Message: &Message{ID: id, Type: "private", Content: content, SenderID: testUserID}}
}

func TestStreamCallLinkGetsTheMicOffer(t *testing.T) {
	f := newBotFixture(t)
	f.send(Event{Type: "message", Message: &f.reactedStream})
	wantCalls(t, f.zulipCalls(), "POST /api/v1/messages/100/reactions "+micEmoji)
	if n := len(f.jitsi.requests()); n != 0 {
		t.Errorf("recorder got %d requests; want 0", n)
	}
}

func TestOwnMessagesAndReactionsAreIgnored(t *testing.T) {
	f := newBotFixture(t)
	own := f.reactedStream
	own.SenderID = testBotID
	f.send(Event{Type: "message", Message: &own})
	f.send(Event{Type: "reaction", Op: "add", UserID: testBotID, MessageID: 100, EmojiName: micEmoji})
	wantCalls(t, f.zulipCalls())
	if n := len(f.jitsi.requests()); n != 0 {
		t.Errorf("recorder got %d requests; want 0", n)
	}
}

func TestClickStartsASignedJitsiJob(t *testing.T) {
	f := newBotFixture(t)
	f.click()

	reqs := f.jitsi.requests()
	if len(reqs) != 1 {
		t.Fatalf("recorder got %d requests; want 1", len(reqs))
	}
	got := reqs[0]
	want := recordingRequest{ID: "100", URL: testRoomURL, CallbackURL: "http://bot.example.com:8080/events",
		Meta: map[string]any{"message_id": float64(100)}, DisplayName: "NoteTaker",
		JoinTimeoutS: 600, MaxDurationS: 14400, EmptyGraceS: 60}
	if g, w := mustJSON(t, got), mustJSON(t, want); g != w {
		t.Errorf("POST /recordings body\n%s\nwant\n%s", g, w)
	}
	wantCalls(t, f.zulipCalls(), "POST /api/v1/messages/100/reactions "+recordingEmoji)

	j := f.job(t, "100")
	if j.State != jobRunning || j.Recorder != recorderJitsi || j.URL != testRoomURL ||
		j.Stream != testStream || j.Topic != testTopic || j.DMUserID != 0 || j.MessageID != 100 {
		t.Errorf("job = %+v", j)
	}
	if d := j.Deadline.Sub(j.RequestedAt); d != (600+14400)*time.Second+watchdogSlack {
		t.Errorf("deadline is %v after the request", d)
	}
}

func TestSecondClickWhileRunningIsANoOp(t *testing.T) {
	f := newBotFixture(t)
	f.click()
	f.click()
	if n := len(f.jitsi.requests()); n != 1 {
		t.Errorf("recorder got %d requests; want 1", n)
	}
	wantCalls(t, f.zulipCalls(), "POST /api/v1/messages/100/reactions "+recordingEmoji)
}

func TestJitsiDMStartsRightAway(t *testing.T) {
	f := newBotFixture(t)
	f.send(dm(200, "please record "+testRoomURL+"."))
	reqs := f.jitsi.requests()
	if len(reqs) != 1 || reqs[0].ID != "200" || reqs[0].URL != testRoomURL {
		t.Fatalf("recorder got %+v", reqs)
	}
	wantCalls(t, f.zulipCalls(), "POST /api/v1/messages/200/reactions "+recordingEmoji)
	if j := f.job(t, "200"); j.DMUserID != testUserID || j.Stream != "" || j.Topic != "" {
		t.Errorf("job = %+v", j)
	}
}

func TestMeetDMDropsTheQueryString(t *testing.T) {
	f := newBotFixture(t)
	f.send(dm(300, testMeet+"?authuser=0&pli=1"))
	reqs := f.meet.requests()
	if len(reqs) != 1 || reqs[0].URL != testMeet || reqs[0].JoinTimeoutS != 1200 {
		t.Fatalf("meet recorder got %+v", reqs)
	}
	if n := len(f.jitsi.requests()); n != 0 {
		t.Errorf("jitsi recorder got %d requests; want 0", n)
	}
	wantCalls(t, f.zulipCalls(), "POST /api/v1/messages/300/reactions "+recordingEmoji)
	if j := f.job(t, "300"); j.Recorder != recorderMeet || j.URL != testMeet || j.DMUserID != testUserID {
		t.Errorf("job = %+v", j)
	}
}

func TestMeetLinkInStreamIsIgnored(t *testing.T) {
	f := newBotFixture(t)
	f.send(Event{Type: "message", Message: &Message{ID: 400, Type: "stream", Content: testMeet,
		DisplayRecipient: testStream, Subject: testTopic, SenderID: testUserID}})
	wantCalls(t, f.zulipCalls())
	if n := len(f.meet.requests()); n != 0 {
		t.Errorf("meet recorder got %d requests; want 0", n)
	}
}

func TestRecorder5xxIsRetriedThenReported(t *testing.T) {
	f := newBotFixture(t, http.StatusServiceUnavailable)
	f.click()
	if n := len(f.jitsi.requests()); n != 1+len(recorderRetryDelays) {
		t.Errorf("recorder got %d requests; want %d", n, 1+len(recorderRetryDelays))
	}
	wantFailure(t, f)
}

func TestRecorder5xxThenAcceptedRecovers(t *testing.T) {
	f := newBotFixture(t, http.StatusBadGateway, http.StatusAccepted)
	f.click()
	if n := len(f.jitsi.requests()); n != 2 {
		t.Errorf("recorder got %d requests; want 2", n)
	}
	if j := f.job(t, "100"); j.State != jobRunning {
		t.Errorf("state = %q; want running", j.State)
	}
}

func TestRecorder4xxIsReportedAtOnce(t *testing.T) {
	f := newBotFixture(t, http.StatusUnprocessableEntity)
	f.click()
	if n := len(f.jitsi.requests()); n != 1 {
		t.Errorf("recorder got %d requests; want 1", n)
	}
	wantFailure(t, f)

	// No recorder ever took the job, so a new click tries again.
	f.click()
	if n := len(f.jitsi.requests()); n != 2 {
		t.Errorf("after a re-click the recorder got %d requests; want 2", n)
	}
}

func wantFailure(t *testing.T, f *botFixture) {
	t.Helper()
	wantCalls(t, f.zulipCalls(),
		"POST /api/v1/messages/100/reactions "+recordingEmoji,
		"POST /api/v1/messages "+noteRecorderFailed,
		"DELETE /api/v1/messages/100/reactions "+recordingEmoji)
	note := f.zulip.requests()[2].form
	if note.Get("to") != `"`+testStream+`"` || note.Get("topic") != testTopic {
		t.Errorf("note went to %v", note)
	}
	if j := f.job(t, "100"); j.State != jobFailed || j.Error != "recorder_failed" {
		t.Errorf("job = %+v", j)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
