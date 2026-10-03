package main

import (
	"strings"
	"testing"
)

// setRequired sets the required variables and clears every optional one, so
// each test starts from a known environment.
func setRequired(t *testing.T) {
	t.Helper()
	for _, n := range []string{"LISTEN_ADDR", "PUBLIC_URL", "DATA_DIR", "LOG_LEVEL"} {
		t.Setenv(n, "")
	}
	t.Setenv("ZULIP_SITE", "https://zulip.example.com/")
	t.Setenv("ZULIP_BOT_EMAIL", "bot@example.com")
	t.Setenv("ZULIP_BOT_API_KEY", "test-key")
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
