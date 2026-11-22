package outbox

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"enclave/api/internal/platform/testenv"
)

func TestBackoffDoublesWithJitter(t *testing.T) {
	ceilings := map[int]time.Duration{1: 2 * time.Second, 2: 4 * time.Second, 5: 32 * time.Second, 12: time.Hour, 50: time.Hour}
	for attempts, ceiling := range ceilings {
		seen := map[time.Duration]bool{}
		for range 50 {
			got := Backoff(attempts)
			if got < ceiling/2 || got > ceiling {
				t.Fatalf("Backoff(%d) = %v, want between %v and %v", attempts, got, ceiling/2, ceiling)
			}
			seen[got] = true
		}
		if len(seen) < 2 {
			t.Errorf("Backoff(%d) always returned the same delay; jobs failing together would retry together", attempts)
		}
	}
}

// uniqueKind keeps jobs of concurrently running test packages apart.
func uniqueKind(name string) string {
	return "test." + name + "." + rand.Text()[:8]
}

func newTestWorker(pool *pgxpool.Pool) *Worker {
	return NewWorker(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func jobExists(t *testing.T, pool *pgxpool.Pool, kind string) bool {
	t.Helper()
	_, exists := loadJob(t, pool, kind)
	return exists
}

func enqueue(t *testing.T, pool *pgxpool.Pool, kind string, payload any) {
	t.Helper()
	job, err := NewJob(kind, payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		return Enqueue(context.Background(), tx, job)
	}); err != nil {
		t.Fatal(err)
	}
}

type jobRow struct {
	attempts int
	lastErr  *string
	dead     bool
	delayed  bool
}

func loadJob(t *testing.T, pool *pgxpool.Pool, kind string) (jobRow, bool) {
	t.Helper()
	var row jobRow
	err := pool.QueryRow(context.Background(),
		`SELECT attempts, last_error, dead_at IS NOT NULL, run_after > now() FROM outbox_jobs WHERE kind = $1`, kind,
	).Scan(&row.attempts, &row.lastErr, &row.dead, &row.delayed)
	if errors.Is(err, pgx.ErrNoRows) {
		return jobRow{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return row, true
}

func TestSuccessfulJobRunsAndIsDeleted(t *testing.T) {
	pool := testenv.Postgres(t)
	kind := uniqueKind("success")
	enqueue(t, pool, kind, map[string]string{"hello": "world"})

	var got map[string]string
	w := newTestWorker(pool)
	w.Handle(kind, func(_ context.Context, payload json.RawMessage) error {
		return json.Unmarshal(payload, &got)
	})

	processed, err := w.RunOnce(context.Background())
	if err != nil || !processed {
		t.Fatalf("RunOnce = %v, %v", processed, err)
	}
	if got["hello"] != "world" {
		t.Fatalf("payload = %v", got)
	}
	if _, exists := loadJob(t, pool, kind); exists {
		t.Fatal("a successful job must be removed")
	}
}

func TestFailingJobIsRetriedLaterThenMarkedDead(t *testing.T) {
	pool := testenv.Postgres(t)
	kind := uniqueKind("failing")
	enqueue(t, pool, kind, struct{}{})

	w := newTestWorker(pool)
	w.maxAttempts = 2
	w.Handle(kind, func(context.Context, json.RawMessage) error { return errors.New("storage unavailable") })

	if _, err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	row, _ := loadJob(t, pool, kind)
	if row.attempts != 1 || !row.delayed || row.dead || row.lastErr == nil || *row.lastErr != "storage unavailable" {
		t.Fatalf("after first failure: %+v", row)
	}

	// The job is not due yet, so the worker must not pick it up again immediately.
	if processed, _ := w.RunOnce(context.Background()); processed {
		t.Fatal("a failed job must wait for its backoff")
	}

	if _, err := pool.Exec(context.Background(), `UPDATE outbox_jobs SET run_after = now() WHERE kind = $1`, kind); err != nil {
		t.Fatal(err)
	}
	if _, err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	row, _ = loadJob(t, pool, kind)
	if row.attempts != 2 || !row.dead {
		t.Fatalf("after reaching max attempts: %+v", row)
	}
}

func TestWorkerIgnoresKindsItDoesNotHandle(t *testing.T) {
	pool := testenv.Postgres(t)
	foreign := uniqueKind("foreign")
	enqueue(t, pool, foreign, struct{}{})
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM outbox_jobs WHERE kind = $1`, foreign) })

	w := newTestWorker(pool)
	w.Handle(uniqueKind("mine"), func(context.Context, json.RawMessage) error { return nil })
	if processed, _ := w.RunOnce(context.Background()); processed {
		t.Fatal("worker claimed a job it has no handler for")
	}
}

func TestConcurrentWorkersRunEachJobOnce(t *testing.T) {
	pool := testenv.Postgres(t)
	kind := uniqueKind("concurrent")
	const jobs = 20
	for i := range jobs {
		enqueue(t, pool, kind, map[string]int{"n": i})
	}

	var runs atomic.Int32
	handler := func(context.Context, json.RawMessage) error {
		runs.Add(1)
		time.Sleep(5 * time.Millisecond) // hold the row lock long enough to overlap
		return nil
	}

	var wg sync.WaitGroup
	for range 4 {
		w := newTestWorker(pool)
		w.Handle(kind, handler)
		wg.Go(func() {
			for {
				processed, err := w.RunOnce(context.Background())
				if err != nil {
					t.Error(err)
					return
				}
				if !processed {
					return
				}
			}
		})
	}
	wg.Wait()

	if got := runs.Load(); got != jobs {
		t.Fatalf("handler ran %d times for %d jobs; SKIP LOCKED must give each job to exactly one worker", got, jobs)
	}
}

func TestHandlerRunsOutsideTransactionAndHoldsNoConnection(t *testing.T) {
	pool := testenv.Postgres(t)
	kind := uniqueKind("no-connection")
	enqueue(t, pool, kind, struct{}{})

	acquiredDuringHandler := int32(-1)
	w := newTestWorker(pool)
	w.Handle(kind, func(context.Context, json.RawMessage) error {
		acquiredDuringHandler = pool.Stat().AcquiredConns()
		return nil
	})
	if _, err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if acquiredDuringHandler != 0 {
		t.Fatalf("%d database connections were held while the handler ran, want 0", acquiredDuringHandler)
	}
}

func TestExpiredLeaseIsTakenOverByAnotherWorker(t *testing.T) {
	pool := testenv.Postgres(t)
	kind := uniqueKind("crashed")
	enqueue(t, pool, kind, struct{}{})

	// Worker A claims the job and then "crashes": it never finishes.
	crashed := newTestWorker(pool)
	crashed.Handle(kind, func(context.Context, json.RawMessage) error { return nil })
	if _, found, err := crashed.claim(context.Background()); err != nil || !found {
		t.Fatalf("claim = %v, %v", found, err)
	}

	survivor := newTestWorker(pool)
	survivor.Handle(kind, func(context.Context, json.RawMessage) error { return nil })
	if processed, _ := survivor.RunOnce(context.Background()); processed {
		t.Fatal("a job under a valid lease must not be taken")
	}

	if _, err := pool.Exec(context.Background(), `UPDATE outbox_jobs SET locked_until = now() - interval '1 second' WHERE kind = $1`, kind); err != nil {
		t.Fatal(err)
	}
	if processed, err := survivor.RunOnce(context.Background()); err != nil || !processed {
		t.Fatalf("expired lease: RunOnce = %v, %v; want the job to be taken over", processed, err)
	}
	if jobExists(t, pool, kind) {
		t.Fatal("the survivor should have finished the job")
	}
}

func TestWorkerThatLostItsLeaseCannotFinishTheJob(t *testing.T) {
	pool := testenv.Postgres(t)
	kind := uniqueKind("fencing")
	enqueue(t, pool, kind, struct{}{})
	ctx := context.Background()

	slow := newTestWorker(pool)
	slow.Handle(kind, func(context.Context, json.RawMessage) error { return nil })
	slowJob, _, err := slow.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// The slow worker's lease expires and another worker claims the job with a new token.
	if _, err := pool.Exec(ctx, `UPDATE outbox_jobs SET locked_until = now() - interval '1 second' WHERE kind = $1`, kind); err != nil {
		t.Fatal(err)
	}
	other := newTestWorker(pool)
	other.Handle(kind, func(context.Context, json.RawMessage) error { return nil })
	otherJob, _, err := other.claim(ctx)
	if err != nil || otherJob.id != slowJob.id {
		t.Fatalf("second claim = %+v, %v", otherJob, err)
	}

	// The slow worker finishes late: its old token must not delete the job.
	if err := slow.finish(ctx, slowJob, nil); err != nil {
		t.Fatal(err)
	}
	if !jobExists(t, pool, kind) {
		t.Fatal("a worker with an expired lease deleted a job another worker owns")
	}
	if err := other.finish(ctx, otherJob, nil); err != nil {
		t.Fatal(err)
	}
	if jobExists(t, pool, kind) {
		t.Fatal("the current lease holder must be able to finish the job")
	}
}

func TestHandlerTimeoutReschedulesTheJob(t *testing.T) {
	pool := testenv.Postgres(t)
	kind := uniqueKind("hanging")
	enqueue(t, pool, kind, struct{}{})

	w := newTestWorker(pool)
	w.handlerTimeout = 50 * time.Millisecond
	w.Handle(kind, func(ctx context.Context, _ json.RawMessage) error {
		<-ctx.Done() // a dependency that never answers
		return ctx.Err()
	})

	start := time.Now()
	if _, err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("RunOnce took %v; a hanging handler must be cut off by its timeout", elapsed)
	}
	row, _ := loadJob(t, pool, kind)
	if row.lastErr == nil || !strings.Contains(*row.lastErr, "deadline exceeded") || !row.delayed {
		t.Fatalf("job after timeout: %+v", row)
	}
}

func TestHeartbeatKeepsALongJobLeased(t *testing.T) {
	pool := testenv.Postgres(t)
	kind := uniqueKind("long")
	enqueue(t, pool, kind, struct{}{})

	release := make(chan struct{})
	long := newTestWorker(pool)
	long.leaseDuration = 300 * time.Millisecond // renewed every 100 ms
	long.Handle(kind, func(context.Context, json.RawMessage) error {
		<-release
		return nil
	})

	done := make(chan error, 1)
	go func() {
		_, err := long.RunOnce(context.Background())
		done <- err
	}()

	// Well past the original 300 ms lease, the job must still be held.
	time.Sleep(900 * time.Millisecond)
	other := newTestWorker(pool)
	other.Handle(kind, func(context.Context, json.RawMessage) error { return nil })
	if processed, _ := other.RunOnce(context.Background()); processed {
		t.Fatal("another worker took a job whose lease was being renewed")
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if jobExists(t, pool, kind) {
		t.Fatal("the long job should be finished")
	}
}
