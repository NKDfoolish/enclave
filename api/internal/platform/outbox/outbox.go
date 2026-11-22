// Package outbox implements the transactional outbox: work that must follow a database change
// (delete a stored file, validate an upload) is written as a job in the same transaction as
// the change, then a background worker runs it. A commit therefore always comes with its
// follow-up work, and a crash between "commit" and "do the work" loses nothing: the job is
// still in the table and is retried.
//
// Workers lease jobs instead of holding a transaction while the handler runs. Claiming a job
// writes a lease token and deadline and commits at once, so no database connection is held
// during slow work. A lease that is not renewed expires, and another worker takes the job.
// Delivery is therefore at-least-once: handlers must be idempotent.
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Job is a unit of background work. Kind selects the handler; Payload is its JSON input.
type Job struct {
	Kind    string
	Payload json.RawMessage
}

func NewJob(kind string, payload any) (Job, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return Job{}, fmt.Errorf("encode %s payload: %w", kind, err)
	}
	return Job{Kind: kind, Payload: encoded}, nil
}

// Enqueue writes job inside tx. It becomes visible to workers only if tx commits.
func Enqueue(ctx context.Context, tx pgx.Tx, job Job) error {
	if _, err := tx.Exec(ctx, `INSERT INTO outbox_jobs (kind, payload) VALUES ($1, $2)`, job.Kind, job.Payload); err != nil {
		return fmt.Errorf("enqueue %s job: %w", job.Kind, err)
	}
	return nil
}

// Handler runs one job. Returning an error schedules a retry with backoff. Handlers must be
// idempotent: a job can run more than once (see the package comment).
type Handler func(ctx context.Context, payload json.RawMessage) error

const (
	defaultPollInterval = time.Second
	defaultMaxAttempts  = 10
	// The handler timeout is shorter than the lease, so a slow handler is cancelled while its
	// lease is still valid and nobody else has started the same job.
	defaultHandlerTimeout = 2 * time.Minute
	defaultLeaseDuration  = 5 * time.Minute
	maxBackoff            = time.Hour
)

type Worker struct {
	pool           *pgxpool.Pool
	logger         *slog.Logger
	handlers       map[string]Handler
	pollInterval   time.Duration
	maxAttempts    int
	handlerTimeout time.Duration
	leaseDuration  time.Duration
}

func NewWorker(pool *pgxpool.Pool, logger *slog.Logger) *Worker {
	return &Worker{
		pool:           pool,
		logger:         logger,
		handlers:       map[string]Handler{},
		pollInterval:   defaultPollInterval,
		maxAttempts:    defaultMaxAttempts,
		handlerTimeout: defaultHandlerTimeout,
		leaseDuration:  defaultLeaseDuration,
	}
}

// Handle registers the handler for kind. The worker only claims jobs of registered kinds, so
// several workers with different handlers can share the table.
func (w *Worker) Handle(kind string, handler Handler) {
	w.handlers[kind] = handler
}

// Run processes due jobs until ctx is cancelled. A job that is running when ctx is cancelled
// finishes (or times out) first, so shutdown never abandons work mid-way.
func (w *Worker) Run(ctx context.Context) {
	w.logger.Info("outbox worker started", "kinds", w.kinds())
	for {
		processed, err := w.RunOnce(context.WithoutCancel(ctx))
		if err != nil {
			w.logger.Error("outbox worker", "err", err)
		}
		if processed && ctx.Err() == nil {
			continue // more work may be waiting; poll again immediately
		}
		select {
		case <-ctx.Done():
			w.logger.Info("outbox worker stopped")
			return
		case <-time.After(w.pollInterval):
		}
	}
}

// leasedJob is a job this worker currently holds.
type leasedJob struct {
	id       int64
	kind     string
	payload  json.RawMessage
	attempts int
	token    uuid.UUID
}

// RunOnce leases at most one due job, runs its handler and records the outcome. It reports
// whether a job was processed.
func (w *Worker) RunOnce(ctx context.Context) (bool, error) {
	job, found, err := w.claim(ctx)
	if err != nil || !found {
		return false, err
	}
	handlerErr := w.runHandler(ctx, job)
	return true, w.finish(ctx, job, handlerErr)
}

// claim leases the oldest due job of a registered kind in one short statement and commits.
// SKIP LOCKED lets concurrent workers pass over a row another worker is claiming right now;
// the lease (locked_until) then keeps the job away from them until it expires.
//
// attempts counts claims, not failures: a job that crashes its worker every time still runs
// out of attempts and ends up dead instead of looping forever.
func (w *Worker) claim(ctx context.Context) (leasedJob, bool, error) {
	job := leasedJob{token: uuid.New()}
	err := w.pool.QueryRow(ctx, `
		UPDATE outbox_jobs
		SET locked_until = now() + $2, lease_token = $3, attempts = attempts + 1
		WHERE id = (
			SELECT id FROM outbox_jobs
			WHERE run_after <= now() AND dead_at IS NULL AND kind = ANY($1)
			  AND (locked_until IS NULL OR locked_until < now())
			ORDER BY id
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, kind, payload, attempts`,
		w.kinds(), w.leaseDuration, job.token,
	).Scan(&job.id, &job.kind, &job.payload, &job.attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return leasedJob{}, false, nil
	}
	if err != nil {
		return leasedJob{}, false, fmt.Errorf("claim job: %w", err)
	}
	return job, true, nil
}

// runHandler runs the job's handler with a timeout, renewing the lease while it works. If the
// lease is lost (another worker took the job after it expired), the handler is cancelled.
func (w *Worker) runHandler(ctx context.Context, job leasedJob) error {
	handlerCtx, cancel := context.WithTimeout(ctx, w.handlerTimeout)
	defer cancel()

	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		w.renewLease(handlerCtx, cancel, job)
	}()

	err := w.handlers[job.kind](handlerCtx, job.payload)
	// Stop the heartbeat before returning. Order matters: the heartbeat only exits once
	// handlerCtx is cancelled, so cancel first, then wait.
	cancel()
	<-heartbeatDone
	return err
}

// renewLease extends the lease every third of its duration until ctx ends. Losing the lease
// cancels the handler: its result would be discarded anyway.
func (w *Worker) renewLease(ctx context.Context, cancelHandler context.CancelFunc, job leasedJob) {
	ticker := time.NewTicker(w.leaseDuration / 3)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tag, err := w.pool.Exec(ctx,
				`UPDATE outbox_jobs SET locked_until = now() + $3 WHERE id = $1 AND lease_token = $2`,
				job.id, job.token, w.leaseDuration)
			if err != nil {
				continue // try again next tick; the lease has margin left
			}
			if tag.RowsAffected() == 0 {
				w.logger.Warn("outbox lease lost, cancelling handler", "job_id", job.id, "kind", job.kind)
				cancelHandler()
				return
			}
		}
	}
}

// finish records the handler's outcome. Every statement requires our lease token (a fencing
// token): a worker whose lease expired and was taken over cannot delete or overwrite the job
// the new holder is working on.
func (w *Worker) finish(ctx context.Context, job leasedJob, handlerErr error) error {
	if handlerErr == nil {
		tag, err := w.pool.Exec(ctx, `DELETE FROM outbox_jobs WHERE id = $1 AND lease_token = $2`, job.id, job.token)
		if err != nil {
			return fmt.Errorf("delete finished job %d: %w", job.id, err)
		}
		w.warnIfLeaseLost(tag.RowsAffected(), job)
		return nil
	}

	if job.attempts >= w.maxAttempts {
		w.logger.Error("outbox job failed permanently", "job_id", job.id, "kind", job.kind, "attempts", job.attempts, "err", handlerErr)
		tag, err := w.pool.Exec(ctx, `
			UPDATE outbox_jobs
			SET last_error = $3, dead_at = now(), locked_until = NULL, lease_token = NULL
			WHERE id = $1 AND lease_token = $2`,
			job.id, job.token, handlerErr.Error())
		if err != nil {
			return fmt.Errorf("mark job %d dead: %w", job.id, err)
		}
		w.warnIfLeaseLost(tag.RowsAffected(), job)
		return nil
	}

	delay := Backoff(job.attempts)
	w.logger.Warn("outbox job failed, will retry", "job_id", job.id, "kind", job.kind, "attempts", job.attempts, "retry_in", delay.String(), "err", handlerErr)
	tag, err := w.pool.Exec(ctx, `
		UPDATE outbox_jobs
		SET last_error = $3, run_after = now() + $4, locked_until = NULL, lease_token = NULL
		WHERE id = $1 AND lease_token = $2`,
		job.id, job.token, handlerErr.Error(), delay)
	if err != nil {
		return fmt.Errorf("reschedule job %d: %w", job.id, err)
	}
	w.warnIfLeaseLost(tag.RowsAffected(), job)
	return nil
}

func (w *Worker) warnIfLeaseLost(rowsAffected int64, job leasedJob) {
	if rowsAffected == 0 {
		w.logger.Warn("outbox lease lost before finishing; another worker owns the job now", "job_id", job.id, "kind", job.kind)
	}
}

// Backoff returns how long to wait before retry number attempts: the delay doubles each time
// (2s, 4s, 8s, ...) up to an hour, so a broken dependency is not hammered while it recovers.
//
// Half of the delay is random ("equal jitter"). Without it, jobs that failed together would
// all retry at the same instant and hit the recovering dependency in synchronised waves; the
// fixed half keeps a minimum pause so a retry is never immediate.
func Backoff(attempts int) time.Duration {
	base := maxBackoff
	if attempts <= 11 { // 2^12 s already exceeds an hour
		base = min(time.Duration(1<<attempts)*time.Second, maxBackoff)
	}
	half := base / 2
	return half + rand.N(half+1) //nolint:gosec // jitter needs spread, not unpredictability
}

func (w *Worker) kinds() []string {
	kinds := make([]string, 0, len(w.handlers))
	for kind := range w.handlers {
		kinds = append(kinds, kind)
	}
	return kinds
}
