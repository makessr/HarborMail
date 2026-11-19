package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestSpamFeedbackHonorsControllerPauseAcrossWorkers(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		testSpamFeedbackControllerPause(t, "sqlite", filepath.Join(t.TempDir(), "mail.db"))
	})
	if dsn := os.Getenv("HARBOR_TEST_MYSQL_DSN"); dsn != "" {
		t.Run("mysql", func(t *testing.T) { testSpamFeedbackControllerPause(t, "mysql", dsn) })
	}
}

func testSpamFeedbackControllerPause(t *testing.T, driver, dsn string) {
	db, err := openDB(driver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	otherDB, err := openDB(driver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer otherDB.Close()

	owner := token()[:16] + "@feedback.harbor.test"
	var requests atomic.Int32
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer controller.Close()
	newTrainer := func() *rspamdTrainer {
		return &rspamdTrainer{origin: controller.URL, allowed: map[string]bool{owner: true},
			accountArgs: []any{owner}, client: controller.Client()}
	}
	first, second := &app{db: db, spamTrainer: newTrainer()}, &app{db: otherDB, spamTrainer: newTrainer()}
	for _, subject := range []string{"first feedback", "second feedback"} {
		result, err := db.Exec(`INSERT INTO letters(owner,sender,recipient,subject,body,stamp,folder,origin,raw)
			VALUES(?,?,?,?,'body',1,'junk','inbox',?)`, owner, "sender@harbor.test", owner, subject,
			[]byte("Subject: "+subject+"\r\n\r\nSynthetic body"))
		if err != nil {
			t.Fatal(err)
		}
		id, err := result.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO spam_feedback(letter_id,owner,sample_class,revision,state,due)
			VALUES(?,?,'spam',1,'pending',0)`, id, owner); err != nil {
			t.Fatal(err)
		}
	}
	job, err := first.claimSpamFeedback(t.Context())
	if err != nil || job == nil {
		t.Fatalf("first worker has no sample: %v", err)
	}
	if err := first.processSpamFeedback(job); err != nil {
		t.Fatal(err)
	}
	next, err := second.claimSpamFeedback(t.Context())
	if err != nil || next != nil || requests.Load() != 1 {
		t.Fatalf("second worker should wait after Retry-After: job=%v, requests=%d, err=%v", next != nil, requests.Load(), err)
	}
	if err := second.pauseSpamController(t.Context(), time.Now().Add(time.Second).Unix()); err != nil {
		t.Fatal(err)
	}
	var until int64
	if err := db.QueryRow(`SELECT pause_until FROM spam_controller_pauses WHERE controller=?`,
		first.spamTrainer.controllerID()).Scan(&until); err != nil || until < time.Now().Add(50*time.Second).Unix() {
		t.Fatalf("the shorter pause replaced the existing deadline: %d (%v)", until, err)
	}
	if _, err := db.Exec(`UPDATE spam_controller_pauses SET pause_until=? WHERE controller=?`,
		time.Now().Unix(), first.spamTrainer.controllerID()); err != nil {
		t.Fatal(err)
	}
	next, err = second.claimSpamFeedback(t.Context())
	if err != nil || next == nil || next.id == job.id || next.attempts != 1 {
		t.Fatalf("the next pending sample should resume without consuming a retry: %+v (%v)", next, err)
	}
	if err := second.processSpamFeedback(next); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := db.QueryRow(`SELECT state FROM spam_feedback WHERE letter_id=?`, next.id).Scan(&state); err != nil ||
		state != "done" || requests.Load() != 2 {
		t.Fatalf("resumed sample: state=%q requests=%d err=%v", state, requests.Load(), err)
	}
}

func TestSpamFeedbackRetryAfter(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, header string
		want         time.Duration
	}{
		{"seconds", "120", 120 * time.Second},
		{"http date", now.Add(2 * time.Minute).Format(http.TimeFormat), 2 * time.Minute},
		{"short interval", "5", 5 * time.Second},
		{"expired http date", now.Add(-time.Minute).Format(http.TimeFormat), 0},
		{"invalid header", "later", 0},
		{"cap", "3601", time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := spamFeedbackRetryAfter(tc.header, now); got != tc.want {
				t.Fatalf("retry delay = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestRspamdLearnerUsesRetryAfterOnlyOnRetryableResponses(t *testing.T) {
	const owner = "sample@harbor.test"
	for _, tc := range []struct {
		status int
		header string
		want   spamTrainingRetry
	}{
		{http.StatusTooManyRequests, "60", spamTrainingRetry{sample: time.Minute, controller: time.Minute}},
		{http.StatusServiceUnavailable, "5", spamTrainingRetry{sample: 20 * time.Second, controller: 5 * time.Second}},
		{http.StatusServiceUnavailable, "", spamTrainingRetry{sample: 20 * time.Second}},
		{http.StatusForbidden, "60", spamTrainingRetry{}},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", tc.header)
				w.WriteHeader(tc.status)
			}))
			defer controller.Close()
			trainer := rspamdTrainer{
				origin: controller.URL, allowed: map[string]bool{owner: true}, client: controller.Client(),
			}
			job := &spamFeedbackJob{owner: owner, class: "spam", raw: []byte("Subject: Synthetic test\r\n\r\nBody")}
			if delay, err := trainer.learn(t.Context(), job, 20*time.Second); err == nil || delay != tc.want {
				t.Fatalf("learn returned (%+v, %v), want (%+v, error)", delay, err, tc.want)
			}
		})
	}
}
