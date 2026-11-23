package mailqueue

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"
	"time"
)

type queueFixture struct {
	calls       []string
	jobs        []int
	recoverErr  error
	claimErr    error
	deadlineErr error
}

func (f *queueFixture) Recover(context.Context) error {
	f.calls = append(f.calls, "recover")
	return f.recoverErr
}

func (f *queueFixture) Claim() (*int, error) {
	f.calls = append(f.calls, "claim")
	if len(f.jobs) == 0 {
		return nil, f.claimErr
	}
	job := f.jobs[0]
	f.jobs = f.jobs[1:]
	return &job, nil
}

func (f *queueFixture) NextDeadline(ctx context.Context, claimAt, recoveryAt int64) (sql.NullInt64, error) {
	f.calls = append(f.calls, "deadline")
	if _, ok := ctx.Deadline(); !ok || claimAt < recoveryAt || recoveryAt == 0 {
		return sql.NullInt64{}, errors.New("invalid probe bounds")
	}
	if f.deadlineErr != nil {
		return sql.NullInt64{}, f.deadlineErr
	}
	return sql.NullInt64{Int64: 123, Valid: true}, nil
}

func TestRun(t *testing.T) {
	const poll = 20 * time.Second
	unavailable := errors.New("store unavailable")
	cases := []struct {
		name     string
		fixture  queueFixture
		calls    []string
		jobs     []int
		interval time.Duration
	}{
		{"drains ready jobs", queueFixture{jobs: []int{1, 2}}, []string{"recover", "claim", "claim", "claim", "deadline", "wait"}, []int{1, 2}, poll},
		{"failed recovery skips claims", queueFixture{recoverErr: unavailable}, []string{"recover", "wait"}, nil, 3 * time.Second},
		{"failed claim skips deadline", queueFixture{claimErr: unavailable}, []string{"recover", "claim", "wait"}, nil, 3 * time.Second},
		{"failed deadline retries", queueFixture{deadlineErr: unavailable}, []string{"recover", "claim", "deadline", "wait"}, nil, 3 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var delivered []int
			Run(ctx, &tc.fixture, func(job *int) error {
				delivered = append(delivered, *job)
				return unavailable
			}, func(_ context.Context, deadline sql.NullInt64, interval time.Duration) {
				tc.fixture.calls = append(tc.fixture.calls, "wait")
				if interval != tc.interval || deadline.Valid != (tc.interval == poll) || deadline.Valid && deadline.Int64 != 123 {
					t.Errorf("unexpected wait interval %v or deadline %+v", interval, deadline)
				}
				cancel()
			}, poll)
			if !reflect.DeepEqual(delivered, tc.jobs) || !reflect.DeepEqual(tc.fixture.calls, tc.calls) {
				t.Fatalf("delivered %v, calls %v", delivered, tc.fixture.calls)
			}
		})
	}
}
