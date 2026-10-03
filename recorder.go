package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// recorderRetryDelays are the pauses between attempts after a network error or
// a 5xx: three retries over ~30 s. A var so tests can shrink it.
var recorderRetryDelays = []time.Duration{5 * time.Second, 10 * time.Second, 15 * time.Second}

var recorderHTTP = &http.Client{
	Timeout: 15 * time.Second,
	// Redirects are not followed in either direction of the contract.
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// sign is the x-recorder-signature value for body (docs/architecture.md §3.1).
func sign(body []byte, secret string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// recordingRequest is the POST /recordings body (§3.2).
type recordingRequest struct {
	ID           string         `json:"id"`
	URL          string         `json:"url"`
	CallbackURL  string         `json:"callback_url"`
	Meta         map[string]any `json:"meta"`
	DisplayName  string         `json:"display_name"`
	JoinTimeoutS int            `json:"join_timeout_s"`
	MaxDurationS int            `json:"max_duration_s"`
	EmptyGraceS  int            `json:"empty_grace_s"`
}

// requestRecording asks the recorder at base to record. 200/202 is success;
// a network error or 5xx is retried per recorderRetryDelays; anything else
// (4xx) fails at once.
func requestRecording(ctx context.Context, base, secret string, rr recordingRequest) error {
	body, err := json.Marshal(rr)
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		status, err := postSigned(ctx, base+"/recordings", secret, body)
		switch {
		case err == nil && (status == http.StatusOK || status == http.StatusAccepted):
			return nil
		case err == nil && status < 500:
			return fmt.Errorf("recorder refused the job: http %d", status)
		case attempt == len(recorderRetryDelays):
			if err != nil {
				return fmt.Errorf("recorder unreachable: %w", err)
			}
			return fmt.Errorf("recorder failing: http %d", status)
		}
		slog.Warn("recorder request failed, retrying", "job", rr.ID, "status", status, "err", err)
		pause(ctx, recorderRetryDelays[attempt])
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

func postSigned(ctx context.Context, url, secret string, body []byte) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-recorder-signature", sign(body, secret))
	resp, err := recorderHTTP.Do(req)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode, nil
}
