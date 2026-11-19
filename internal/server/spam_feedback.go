package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

const spamFeedbackAttempts = 5

var errInvalidSpamFeedback = errors.New("invalid spam feedback destination")

type rspamdTrainer struct {
	origin      string
	password    string
	allowed     map[string]bool
	accountArgs []any
	client      *http.Client
}

func configureSpamTrainer(accounts map[string]bool) (*rspamdTrainer, error) {
	endpoint := strings.TrimSpace(os.Getenv("HARBOR_RSPAMD_TRAIN_URL"))
	if endpoint == "" {
		return nil, nil
	}
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" ||
		u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		(u.EscapedPath() != "" && u.EscapedPath() != "/") {
		return nil, errors.New("HARBOR_RSPAMD_TRAIN_URL must be an HTTP(S) controller origin without credentials, query or fragment")
	}
	password, err := fileSetting("HARBOR_RSPAMD_TRAIN_PASSWORD")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(password) == "" || strings.ContainsAny(password, "\r\n") {
		return nil, errors.New("set HARBOR_RSPAMD_TRAIN_PASSWORD or HARBOR_RSPAMD_TRAIN_PASSWORD_FILE")
	}
	trainer := &rspamdTrainer{
		origin: strings.TrimSuffix(u.String(), "/"), password: password, allowed: make(map[string]bool),
		client: &http.Client{
			Timeout:       15 * time.Second,
			Transport:     &http.Transport{MaxConnsPerHost: 2, MaxIdleConnsPerHost: 2, IdleConnTimeout: 30 * time.Second},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	for address := range strings.SplitSeq(os.Getenv("HARBOR_RSPAMD_TRAIN_ACCOUNTS"), ",") {
		owner := strings.ToLower(strings.TrimSpace(address))
		if owner == "" || trainer.allowed[owner] {
			continue
		}
		if !accounts[owner] {
			return nil, errors.New("HARBOR_RSPAMD_TRAIN_ACCOUNTS must contain configured primary accounts")
		}
		trainer.allowed[owner] = true
		trainer.accountArgs = append(trainer.accountArgs, owner)
	}
	if len(trainer.accountArgs) == 0 {
		return nil, errors.New("HARBOR_RSPAMD_TRAIN_ACCOUNTS must explicitly allow at least one account")
	}
	return trainer, nil
}

func (trainer *rspamdTrainer) allows(owner string) bool {
	return trainer != nil && trainer.allowed[owner]
}

// Called under the mailbox write lock before the move changes the source folder.
func enqueueSpamFeedback(ctx context.Context, tx *sql.Tx, owner string, id int64, destination string) error {
	var folder, origin string
	if err := tx.QueryRowContext(ctx, `SELECT folder,origin FROM letters WHERE owner=? AND id=?`, owner, id).
		Scan(&folder, &origin); err != nil {
		return err
	}
	if origin != "inbox" || folder == "drafts" || folder == "sent" || folder == "trash" || folder == destination {
		return nil
	}
	class := "spam"
	if destination == "inbox" {
		if folder != "junk" {
			return nil
		}
		class = "ham"
	}
	now := time.Now().Unix()
	var previous, state string
	err := tx.QueryRowContext(ctx, `SELECT sample_class,state FROM spam_feedback WHERE owner=? AND letter_id=?`, owner, id).
		Scan(&previous, &state)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = tx.ExecContext(ctx, `INSERT INTO spam_feedback(letter_id,owner,sample_class,revision,state,due)
			VALUES(?,?,?,1,'pending',?)`, id, owner, class, now)
		return err
	case err != nil:
		return err
	case previous == class && state != "failed":
		return nil
	}
	// Keep any live lease so opposite feedback is sent after the in-flight request.
	_, err = tx.ExecContext(ctx, `UPDATE spam_feedback SET sample_class=?,revision=revision+1,state='pending',due=?,attempts=0
		WHERE owner=? AND letter_id=?`, class, now, owner, id)
	return err
}

type spamFeedbackJob struct {
	id, revision int64
	attempts     int
	owner        string
	class        string
	recipient    string
	claim        string
	raw          []byte
}

func (trainer *rspamdTrainer) controllerID() string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(trainer.origin)))
}

const spamControllerReady = ` AND NOT EXISTS (
	SELECT 1 FROM spam_controller_pauses WHERE controller=? AND pause_until>?
)`

func (a *app) claimSpamFeedback(ctx context.Context) (*spamFeedbackJob, error) {
	now := time.Now()
	job := &spamFeedbackJob{claim: token()}
	controller := a.spamTrainer.controllerID()
	args := append([]any{now.Unix(), now.Unix(), controller, now.Unix()}, a.spamTrainer.accountArgs...)
	err := a.db.QueryRowContext(ctx, `SELECT letter_id,owner,revision FROM spam_feedback
		WHERE state='pending' AND due<=? AND lease<=?`+spamControllerReady+` AND owner IN (`+placeholders(len(a.spamTrainer.accountArgs))+`)
		ORDER BY due,letter_id LIMIT 1`, args...).Scan(&job.id, &job.owner, &job.revision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	result, err := a.db.ExecContext(ctx, `UPDATE spam_feedback SET claim=?,lease=?,attempts=attempts+1
		WHERE letter_id=? AND owner=? AND revision=? AND state='pending' AND due<=? AND lease<=?`+spamControllerReady,
		job.claim, now.Add(2*time.Minute).Unix(), job.id, job.owner, job.revision, now.Unix(), now.Unix(), controller, now.Unix())
	if err := requireAffectedRow(result, err); errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	err = a.db.QueryRowContext(ctx, `SELECT f.sample_class,f.attempts,coalesce(l.recipient,''),l.raw
		FROM spam_feedback f LEFT JOIN letters l ON l.id=f.letter_id AND l.owner=f.owner
		WHERE f.letter_id=? AND f.revision=? AND f.claim=?`, job.id, job.revision, job.claim).
		Scan(&job.class, &job.attempts, &job.recipient, &job.raw)
	if errors.Is(err, sql.ErrNoRows) {
		// A newer correction arrived before the original was sent.
		return nil, a.finishSpamFeedback(ctx, job, "pending", now.Unix())
	}
	if err != nil {
		return nil, err
	}
	return job, nil
}

func (a *app) finishSpamFeedback(ctx context.Context, job *spamFeedbackJob, state string, due int64) error {
	_, err := a.db.ExecContext(ctx, `UPDATE spam_feedback SET state=CASE WHEN revision=? THEN ? ELSE state END,
		due=CASE WHEN revision=? THEN ? ELSE due END,lease=0,claim='' WHERE letter_id=? AND owner=? AND claim=?`,
		job.revision, state, job.revision, due, job.id, job.owner, job.claim)
	return err
}

func (a *app) pauseSpamController(ctx context.Context, until int64) error {
	query := `INSERT INTO spam_controller_pauses(controller,pause_until) VALUES(?,?)
		ON CONFLICT(controller) DO UPDATE SET pause_until=MAX(spam_controller_pauses.pause_until,?)`
	if _, isMySQL := a.db.Driver().(*mysql.MySQLDriver); isMySQL {
		query = `INSERT INTO spam_controller_pauses(controller,pause_until) VALUES(?,?)
			ON DUPLICATE KEY UPDATE pause_until=GREATEST(pause_until,?)`
	}
	_, err := a.db.ExecContext(ctx, query, a.spamTrainer.controllerID(), until, until)
	return err
}

func spamFeedbackRetryAfter(value string, now time.Time) time.Duration {
	const maxDelay = time.Hour
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseUint(value, 10, 64); err == nil {
		return time.Duration(min(seconds, uint64(maxDelay/time.Second))) * time.Second
	}
	if until, err := http.ParseTime(value); err == nil {
		return max(0, min(until.Sub(now), maxDelay))
	}
	return 0
}

type spamTrainingRetry struct {
	sample, controller time.Duration
}

func (trainer *rspamdTrainer) learn(ctx context.Context, job *spamFeedbackJob, backoff time.Duration) (spamTrainingRetry, error) {
	if !trainer.allows(job.owner) || len(job.raw) == 0 || (job.class != "spam" && job.class != "ham") {
		return spamTrainingRetry{}, errors.New("sample unavailable or not permitted")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, trainer.origin+"/learn"+job.class, bytes.NewReader(job.raw))
	if err != nil {
		return spamTrainingRetry{}, errors.New("invalid controller request")
	}
	req.Header.Set("Password", trainer.password)
	req.Header.Set("Content-Type", "message/rfc822")
	req.Header.Set("Deliver-To", job.owner)
	if recipient, err := address(job.recipient); err == nil {
		req.Header.Set("Rcpt", recipient)
	}
	response, err := trainer.client.Do(req)
	if err != nil {
		return spamTrainingRetry{sample: backoff}, errors.New("controller connection failed")
	}
	defer response.Body.Close()
	status := response.StatusCode
	if status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500 {
		delay := spamFeedbackRetryAfter(response.Header.Get("Retry-After"), time.Now())
		return spamTrainingRetry{sample: max(backoff, delay), controller: delay}, fmt.Errorf("controller HTTP %d", status)
	}
	const limit = 16 << 10
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return spamTrainingRetry{sample: backoff}, errors.New("controller response interrupted")
	}
	var result struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
	}
	if len(body) <= limit && json.Unmarshal(body, &result) == nil {
		if status == http.StatusOK && result.Success {
			return spamTrainingRetry{}, nil
		}
		if (status == http.StatusAlreadyReported || status == http.StatusNotFound) &&
			strings.HasSuffix(result.Error, " has been already learned as "+job.class+", ignore it") {
			return spamTrainingRetry{}, nil
		}
	}
	return spamTrainingRetry{}, fmt.Errorf("controller rejected sample (HTTP %d)", status)
}

func (a *app) runSpamFeedback(stop context.Context) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for stop.Err() == nil {
		for stop.Err() == nil {
			ctx, cancel := context.WithTimeout(stop, 5*time.Second)
			job, err := a.claimSpamFeedback(ctx)
			cancel()
			if err != nil {
				if stop.Err() == nil {
					log.Print("spam feedback claim: ", err)
				}
				break
			}
			if job == nil {
				break
			}
			if err := a.processSpamFeedback(job); err != nil {
				log.Print("spam feedback update: ", err)
				break
			}
		}
		select {
		case <-stop.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *app) processSpamFeedback(job *spamFeedbackJob) error {
	state, due, pauseUntil := "done", int64(0), int64(0)
	if job.attempts > spamFeedbackAttempts {
		state = "failed"
		log.Printf("spam feedback id=%d: attempt limit reached", job.id)
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		backoff := 20 * time.Second << (job.attempts - 1)
		retry, err := a.spamTrainer.learn(ctx, job, backoff)
		cancel()
		// Due times use whole seconds. Round up so the pause cannot end early.
		now := time.Now().Add(time.Second - time.Nanosecond)
		if retry.controller > 0 {
			pauseUntil = now.Add(retry.controller).Unix()
		}
		if err != nil {
			state = "failed"
			if retry.sample > 0 && job.attempts < spamFeedbackAttempts {
				state = "pending"
				due = now.Add(retry.sample).Unix()
			}
			log.Printf("spam feedback id=%d state=%s: %v", job.id, state, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if pauseUntil != 0 {
		if err := a.pauseSpamController(ctx, pauseUntil); err != nil {
			return err
		}
	}
	return a.finishSpamFeedback(ctx, job, state, due)
}
