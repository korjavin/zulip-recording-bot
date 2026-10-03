package main

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

// Config is the whole runtime configuration. config.go is the single reader of
// os.Getenv — compose passes the values via env_file, so there are no flags and
// no dotenv loading.
type Config struct {
	ZulipSite      string // no trailing slash
	ZulipBotEmail  string
	ZulipBotAPIKey string

	ListenAddr string
	PublicURL  string // base of the callback URLs handed to recorders and the transcriber
	DataDir    string // the bot's own job state only; it never holds audio
	LogLevel   string

	JitsiBaseURL     string // a link under it is a Jitsi call; no trailing slash
	JitsiRecorderURL string // no trailing slash
	MeetRecorderURL  string // no trailing slash
	RecorderSecret   string // HMAC key for x-recorder-signature, both directions
	BotDisplayName   string
	JoinTimeoutS     int // Jitsi
	MeetJoinTimeoutS int // Meet: a guest waits in the lobby until admitted
	MaxDurationS     int
	EmptyGraceS      int
}

// loadConfig reads the environment. It always returns a Config with defaults
// applied so the caller can set up logging from it even when err is non-nil.
// Errors name the offending variables only — never their values.
func loadConfig() (Config, error) {
	var missing, bad []string
	req := func(name string) string {
		v := os.Getenv(name)
		if v == "" {
			missing = append(missing, name)
		}
		return v
	}
	str := func(name, def string) string {
		if v := os.Getenv(name); v != "" {
			return v
		}
		return def
	}
	base := func(name, def string) string { return strings.TrimRight(str(name, def), "/") }
	num := func(name string, def int) int {
		v := os.Getenv(name)
		if v == "" {
			return def
		}
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			bad = append(bad, name)
			return def
		}
		return n
	}
	// Go evaluates function calls in a composite literal left to right, so the
	// missing-variable list comes out in the order below.
	c := Config{
		ZulipSite:      strings.TrimRight(req("ZULIP_SITE"), "/"),
		ZulipBotEmail:  req("ZULIP_BOT_EMAIL"),
		ZulipBotAPIKey: req("ZULIP_BOT_API_KEY"),
		RecorderSecret: req("RECORDER_SECRET"),

		ListenAddr: str("LISTEN_ADDR", ":8080"),
		PublicURL:  base("PUBLIC_URL", "http://localhost:8080"),
		DataDir:    str("DATA_DIR", "/data"),
		LogLevel:   str("LOG_LEVEL", "INFO"),

		JitsiBaseURL:     base("JITSI_BASE_URL", "https://meet.jit.si"),
		JitsiRecorderURL: base("JITSI_RECORDER_URL", "http://jitsi-recorder:8080"),
		MeetRecorderURL:  base("MEET_RECORDER_URL", "http://meet-recorder:8080"),
		BotDisplayName:   str("BOT_DISPLAY_NAME", "NoteTaker"),
		JoinTimeoutS:     num("JOIN_TIMEOUT_S", 600),
		MeetJoinTimeoutS: num("MEET_JOIN_TIMEOUT_S", 1200),
		MaxDurationS:     num("MAX_DURATION_S", 14400),
		EmptyGraceS:      num("EMPTY_GRACE_S", 60),
	}
	var errs []error
	if len(missing) > 0 {
		errs = append(errs, errors.New("missing required environment variables: "+strings.Join(missing, ", ")))
	}
	if len(bad) > 0 {
		errs = append(errs, errors.New("not a positive integer: "+strings.Join(bad, ", ")))
	}
	return c, errors.Join(errs...)
}
