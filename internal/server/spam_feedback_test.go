package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSpamFeedbackRetryDelay(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const backoff = 40 * time.Second
	for _, tc := range []struct {
		name, header string
		want         time.Duration
	}{
		{"seconds", "120", 120 * time.Second},
		{"http date", now.Add(2 * time.Minute).Format(http.TimeFormat), 2 * time.Minute},
		{"default interval", "5", backoff},
		{"expired http date", now.Add(-time.Minute).Format(http.TimeFormat), backoff},
		{"invalid header", "later", backoff},
		{"cap", "3601", time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := spamFeedbackRetryDelay(tc.header, now, backoff); got != tc.want {
				t.Fatalf("retry delay = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestRspamdLearnerUsesRetryAfterOnlyOnRetryableResponses(t *testing.T) {
	const owner = "sample@harbor.test"
	for _, tc := range []struct {
		status int
		want   time.Duration
	}{
		{http.StatusTooManyRequests, time.Minute},
		{http.StatusServiceUnavailable, time.Minute},
		{http.StatusForbidden, 0},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "60")
				w.WriteHeader(tc.status)
			}))
			defer controller.Close()
			trainer := rspamdTrainer{
				origin: controller.URL, allowed: map[string]bool{owner: true}, client: controller.Client(),
			}
			job := &spamFeedbackJob{owner: owner, class: "spam", raw: []byte("Subject: Synthetic test\r\n\r\nBody")}
			if delay, err := trainer.learn(t.Context(), job, 20*time.Second); err == nil || delay != tc.want {
				t.Fatalf("learn returned (%s, %v), want (%s, error)", delay, err, tc.want)
			}
		})
	}
}
