package server

import (
	"context"
	"net/http"
	"runtime"
	"runtime/metrics"
	"time"

	"harbormail/internal/processstats"
)

var processStarted = time.Now()

func (a *app) adminRuntime(w http.ResponseWriter, r *http.Request, owner string) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	probeStarted := time.Now()
	dbReady := a.db.PingContext(ctx) == nil
	probeMillis := float64(time.Since(probeStarted).Microseconds()) / 1000
	var mail *operationalCounts
	if dbReady {
		counts, err := a.operationalCounts(ctx, time.Now().Unix())
		if err == nil {
			mail = &counts
		}
	}
	stats := a.db.Stats()
	samples := []metrics.Sample{
		{Name: "/memory/classes/heap/objects:bytes"},
		{Name: "/gc/cycles/total:gc-cycles"},
	}
	metrics.Read(samples)
	process := processstats.Read()
	now := time.Now()
	reply(w, http.StatusOK, map[string]any{
		"email": owner, "at": now.UnixMilli(), "startedAt": processStarted.UnixMilli(),
		"platform": runtime.GOOS, "process": process, "heapBytes": samples[0].Value.Uint64(),
		"gcCycles": samples[1].Value.Uint64(), "goroutines": runtime.NumGoroutine(),
		"cpuParallelism": runtime.GOMAXPROCS(0), "mail": mail, "database": map[string]any{
			"driver": a.dbDriver, "ready": dbReady, "probeMillis": probeMillis,
			"open": stats.OpenConnections, "inUse": stats.InUse, "idle": stats.Idle,
			"limit": stats.MaxOpenConnections, "waitCount": stats.WaitCount,
			"waitMillis": stats.WaitDuration.Milliseconds(),
		},
	})
}
