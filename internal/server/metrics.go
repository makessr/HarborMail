package server

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"
)

func (a *app) listenMetrics(runtime *serviceRuntime, addr string) error {
	if addr == "" {
		return nil
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen for metrics: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", a.metrics)
	server := &http.Server{
		Handler: mux, ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second,
		IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10,
	}
	log.Printf("Metrics listening on %s", listener.Addr())
	runtime.serveHTTP("metrics", server, listener)
	return nil
}

func (a *app) metrics(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	w.Header().Set("Cache-Control", "no-store")
	stats, err := a.operationalCounts(ctx, time.Now().Unix())
	if err != nil {
		http.Error(w, "metrics unavailable", http.StatusServiceUnavailable)
		return
	}
	activeDKIM, unusableDKIM, err := a.dkimKeyCounts(ctx)
	if err != nil {
		http.Error(w, "metrics unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprintf(w, "# TYPE harbor_outbox_oldest_due_seconds gauge\nharbor_outbox_oldest_due_seconds %d\n", stats.OldestDueSeconds)
	paused := 0
	if a.pauseOutbox || a.recoveryMode {
		paused = 1
	}
	fmt.Fprintf(w, "# TYPE harbor_outbox_paused gauge\nharbor_outbox_paused %d\n", paused)
	fmt.Fprintf(w, "# TYPE harbor_outbox_uncertain_messages gauge\nharbor_outbox_uncertain_messages %d\n", stats.Uncertain)
	fmt.Fprintf(w, "# TYPE harbor_spam_training_failed_samples gauge\nharbor_spam_training_failed_samples %d\n", stats.FailedTraining)
	fmt.Fprintf(w, "# TYPE harbor_dkim_active_domains gauge\nharbor_dkim_active_domains %d\n", activeDKIM)
	fmt.Fprintf(w, "# TYPE harbor_dkim_unusable_domains gauge\nharbor_dkim_unusable_domains %d\n", unusableDKIM)
	inbound := a.inbound.snapshot()
	fmt.Fprintf(w, "# TYPE harbor_inbound_active_messages gauge\nharbor_inbound_active_messages %d\n", inbound.messages)
	fmt.Fprintf(w, "# TYPE harbor_inbound_reserved_bytes gauge\nharbor_inbound_reserved_bytes %d\n", inbound.bytes)
	fmt.Fprintf(w, "# TYPE harbor_inbound_active_scans gauge\nharbor_inbound_active_scans %d\n", inbound.scans)
	fmt.Fprintf(w, "# TYPE harbor_inbound_capacity_deferred_total counter\nharbor_inbound_capacity_deferred_total %d\n", inbound.deferred)
	submission := a.submissions.snapshot()
	fmt.Fprintf(w, "# TYPE harbor_submission_active_messages gauge\nharbor_submission_active_messages %d\n", submission.messages)
	fmt.Fprintf(w, "# TYPE harbor_submission_reserved_bytes gauge\nharbor_submission_reserved_bytes %d\n", submission.bytes)
	fmt.Fprintf(w, "# TYPE harbor_submission_capacity_deferred_total counter\nharbor_submission_capacity_deferred_total %d\n", submission.deferred)
}

func (a *app) dkimKeyCounts(ctx context.Context) (active, unusable int, err error) {
	if a.dkimCipher == nil {
		// Every active key is unusable when this instance has no decryption key.
		err := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM domain_dkim_keys WHERE slot='active'`).Scan(&active)
		return active, active, err
	}
	rows, err := a.db.QueryContext(ctx, `SELECT domain,selector,public_key,sealed_key FROM domain_dkim_keys WHERE slot='active'`)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var domain, selector, public string
		var sealed []byte
		if err := rows.Scan(&domain, &selector, &public, &sealed); err != nil {
			return 0, 0, err
		}
		active++
		if storedDKIMKeyState(a.dkimCipher, domain, selector, public, sealed) != "match" {
			unusable++
		}
	}
	return active, unusable, rows.Err()
}

type operationalCounts struct {
	OldestDueSeconds int64 `json:"oldestDueSeconds"`
	Uncertain        int64 `json:"uncertain"`
	FailedTraining   int64 `json:"failedTraining"`
}

func (a *app) operationalCounts(ctx context.Context, now int64) (operationalCounts, error) {
	var stats operationalCounts
	var oldest int64
	// An active claim only needs recovery after its lease expires.
	err := a.db.QueryRowContext(ctx, `SELECT COALESCE(MIN(deadline), ?) FROM (
		SELECT MIN(due) AS deadline FROM outbox WHERE state IN ('queued','scheduled','undo') AND due<=?
		UNION ALL
		SELECT MIN(lease) FROM outbox WHERE state IN ('preparing','sending') AND lease>0 AND lease<=?
	) waiting`, now, now, now).Scan(&oldest)
	if err == nil {
		// Recipient-level uncertainty can coexist with retries for other addresses.
		// Count the message once, even if both levels report uncertainty.
		err = a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM (
			SELECT id FROM outbox WHERE state='uncertain'
			UNION
			SELECT o.id FROM outbox_recipient_progress p JOIN outbox o ON o.id=p.outbox_id
			WHERE p.state='uncertain'
		) unresolved`).Scan(&stats.Uncertain)
	}
	if err == nil {
		err = a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM spam_feedback WHERE state='failed'`).Scan(&stats.FailedTraining)
	}
	stats.OldestDueSeconds = max(0, now-oldest)
	return stats, err
}
