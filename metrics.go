package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"
)

func (a *app) listenMetrics(addr string) error {
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
	go func() { log.Fatal(server.Serve(listener)) }()
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
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprintf(w, "# TYPE harbor_outbox_oldest_due_seconds gauge\nharbor_outbox_oldest_due_seconds %d\n", stats.OldestDueSeconds)
	fmt.Fprintf(w, "# TYPE harbor_outbox_uncertain_messages gauge\nharbor_outbox_uncertain_messages %d\n", stats.Uncertain)
	fmt.Fprintf(w, "# TYPE harbor_spam_training_failed_samples gauge\nharbor_spam_training_failed_samples %d\n", stats.FailedTraining)
}

type operationalCounts struct {
	OldestDueSeconds int64 `json:"oldestDueSeconds"`
	Uncertain        int64 `json:"uncertain"`
	FailedTraining   int64 `json:"failedTraining"`
}

func (a *app) operationalCounts(ctx context.Context, now int64) (operationalCounts, error) {
	var stats operationalCounts
	var oldest int64
	// Appointment and undo windows count only after their delivery time.
	err := a.db.QueryRowContext(ctx, `SELECT COALESCE(MIN(due), ?) FROM outbox
		WHERE state IN ('queued','scheduled','undo') AND due<=?`, now, now).Scan(&oldest)
	if err == nil {
		err = a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbox WHERE state='uncertain'`).Scan(&stats.Uncertain)
	}
	if err == nil {
		err = a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM spam_feedback WHERE state='failed'`).Scan(&stats.FailedTraining)
	}
	stats.OldestDueSeconds = max(0, now-oldest)
	return stats, err
}
