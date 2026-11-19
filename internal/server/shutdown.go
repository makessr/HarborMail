package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/emersion/go-smtp"
)

const shutdownTimeout = 2 * time.Minute

func (a *app) rejectNewSMTPMail() error {
	select {
	case <-a.stopping:
		return &smtp.SMTPError{Code: 421, EnhancedCode: smtp.EnhancedCode{4, 3, 2}, Message: "Harbor Mail is shutting down"}
	default:
		return nil
	}
}

type serviceRuntime struct {
	ctx      context.Context
	cancel   context.CancelFunc
	failures chan error
	pending  sync.WaitGroup
	stops    []func(context.Context)
}

func newServiceRuntime() *serviceRuntime {
	ctx, cancel := context.WithCancel(context.Background())
	return &serviceRuntime{ctx: ctx, cancel: cancel, failures: make(chan error, 1)}
}

func (r *serviceRuntime) serve(name string, serve func() error) <-chan struct{} {
	done := make(chan struct{})
	r.pending.Go(func() {
		defer close(done)
		err := serve()
		if r.ctx.Err() != nil {
			return
		}
		if err == nil {
			err = net.ErrClosed
		}
		select {
		case r.failures <- fmt.Errorf("%s: %w", name, err):
		default:
		}
	})
	return done
}

func (r *serviceRuntime) serveHTTP(name string, server *http.Server, listener net.Listener) {
	r.serve(name, func() error {
		defer listener.Close()
		if server.TLSConfig != nil {
			return server.ServeTLS(listener, "", "")
		}
		return server.Serve(listener)
	})
	r.stops = append(r.stops, func(ctx context.Context) {
		if err := server.Shutdown(ctx); err != nil {
			_ = server.Close()
		}
	})
}

func (r *serviceRuntime) serveSMTP(name string, server *smtp.Server, listener net.Listener) {
	done := r.serve(name, func() error { return server.Serve(listener) })
	r.stops = append(r.stops, func(ctx context.Context) {
		// Stop Accept before waiting for the SMTP library's connection group.
		_ = listener.Close()
		<-done
		if err := server.Shutdown(ctx); err != nil && !errors.Is(err, net.ErrClosed) {
			log.Printf("%s shutdown: %v", name, err)
		}
	})
}

func (r *serviceRuntime) worker(run func(context.Context)) {
	r.pending.Go(func() { run(r.ctx) })
}

func (r *serviceRuntime) shutdown() {
	r.cancel()
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	for _, stop := range r.stops {
		r.pending.Go(func() { stop(ctx) })
	}
	done := make(chan struct{})
	go func() {
		r.pending.Wait()
		close(done)
	}()
	select {
	case <-done:
		log.Print("Harbor Mail stopped")
	case <-ctx.Done():
		log.Print("Harbor Mail shutdown deadline reached")
	}
}
