package main

import (
	"fmt"
	"os"
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
}

// loadConfig reads the environment. It always returns a Config with defaults
// applied so the caller can set up logging from it even when err is non-nil.
// Errors name the offending variables only — never their values.
func loadConfig() (Config, error) {
	var missing []string
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
	// Go evaluates function calls in a composite literal left to right, so the
	// missing-variable list comes out in the order below.
	c := Config{
		ZulipSite:      strings.TrimRight(req("ZULIP_SITE"), "/"),
		ZulipBotEmail:  req("ZULIP_BOT_EMAIL"),
		ZulipBotAPIKey: req("ZULIP_BOT_API_KEY"),

		ListenAddr: str("LISTEN_ADDR", ":8080"),
		PublicURL:  strings.TrimRight(str("PUBLIC_URL", "http://localhost:8080"), "/"),
		DataDir:    str("DATA_DIR", "/data"),
		LogLevel:   str("LOG_LEVEL", "INFO"),
	}
	if len(missing) > 0 {
		return c, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}
	return c, nil
}
