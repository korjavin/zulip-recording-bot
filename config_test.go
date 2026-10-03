package main

import (
	"strings"
	"testing"
)

// setRequired sets the required variables and clears every optional one, so
// each test starts from a known environment.
func setRequired(t *testing.T) {
	t.Helper()
	for _, n := range []string{"LISTEN_ADDR", "PUBLIC_URL", "DATA_DIR", "LOG_LEVEL",
		"JITSI_BASE_URL", "JITSI_RECORDER_URL", "MEET_RECORDER_URL", "BOT_DISPLAY_NAME",
		"JOIN_TIMEOUT_S", "MEET_JOIN_TIMEOUT_S", "MAX_DURATION_S", "EMPTY_GRACE_S", "MIN_RECORDING_S"} {
		t.Setenv(n, "")
	}
	t.Setenv("ZULIP_SITE", "https://zulip.example.com/")
	t.Setenv("ZULIP_BOT_EMAIL", "bot@example.com")
	t.Setenv("ZULIP_BOT_API_KEY", "test-key")
	t.Setenv("RECORDER_SECRET", "test-secret")
}

func TestLoadConfigRecorderDefaults(t *testing.T) {
	setRequired(t)
	c, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if c.JitsiBaseURL != "https://meet.jit.si" || c.BotDisplayName != "NoteTaker" || c.RecorderSecret != "test-secret" ||
		c.JoinTimeoutS != 600 || c.MeetJoinTimeoutS != 1200 || c.MaxDurationS != 14400 || c.EmptyGraceS != 60 || c.MinRecordingS != 15 {
		t.Errorf("got %+v", c)
	}
}

func TestLoadConfigBadNumber(t *testing.T) {
	setRequired(t)
	t.Setenv("MAX_DURATION_S", "4h")
	t.Setenv("EMPTY_GRACE_S", "0")
	_, err := loadConfig()
	if err == nil || err.Error() != "not a positive integer: MAX_DURATION_S, EMPTY_GRACE_S" {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	setRequired(t)
	c, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	for _, tc := range []struct{ name, got, want string }{
		{"ZulipSite", c.ZulipSite, "https://zulip.example.com"},
		{"ListenAddr", c.ListenAddr, ":8080"},
		{"PublicURL", c.PublicURL, "http://localhost:8080"},
		{"DataDir", c.DataDir, "/data"},
		{"LogLevel", c.LogLevel, "INFO"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

func TestLoadConfigOverrides(t *testing.T) {
	setRequired(t)
	t.Setenv("LISTEN_ADDR", ":9090")
	t.Setenv("PUBLIC_URL", "http://bot.example.com:9090/")
	t.Setenv("DATA_DIR", "/state")
	t.Setenv("LOG_LEVEL", "DEBUG")
	c, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if c.ListenAddr != ":9090" || c.PublicURL != "http://bot.example.com:9090" || c.DataDir != "/state" || c.LogLevel != "DEBUG" {
		t.Errorf("got %+v", c)
	}
}

func TestLoadConfigMissingRequired(t *testing.T) {
	setRequired(t)
	t.Setenv("ZULIP_SITE", "")
	t.Setenv("ZULIP_BOT_API_KEY", "")

	_, err := loadConfig()
	if err == nil {
		t.Fatal("loadConfig: want error, got nil")
	}
	want := "missing required environment variables: ZULIP_SITE, ZULIP_BOT_API_KEY"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
	if strings.Contains(err.Error(), "test-key") {
		t.Errorf("error leaks a value: %q", err)
	}
}
