// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	apiv1 "github.com/fluxcd/flux-mirror/api/v1beta1"

	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

// Runner orchestrates EntryMirrors. Entries are processed sequentially so a
// single fat entry can't starve later ones; within an entry, tag jobs are
// fanned out via errgroup.SetLimit(Concurrency).
type Runner struct {
	Concurrency   int
	Retries       int
	PerJobTimeout time.Duration
	Logger        *slog.Logger

	// OnJobFinished, if non-nil, is invoked once per job after Run returns.
	// It runs from the worker goroutine (so concurrent invocations are
	// possible — the callback must be safe to call from multiple goroutines)
	// before the per-tag row is recorded, but after retries and status
	// validation. Used by the cmd layer to drive a progress spinner.
	OnJobFinished func(entry, id, dst string, st apiv1.Status, err error)

	// OnEntryFinished, if non-nil, is invoked from the main goroutine once
	// per entry, after all of that entry's jobs have completed (or its plan
	// failed). Used by the cmd layer to tick the "N/M done" counter on the
	// spinner suffix.
	OnEntryFinished func(entry string)

	// OnPlanError, if non-nil, is invoked when an entry fails at Plan time
	// (e.g. ListTags rejected). It runs from the main goroutine so does not
	// require external synchronization. Plan failures don't reach
	// OnJobFinished — there are no jobs to report on — so this is the only
	// path the cmd layer has to surface them in real time.
	OnPlanError func(entry string, err error)
}

// Run executes all mirrors and returns a Result aggregating their outcomes.
// It does not return an error for ordinary mirror failures — those are
// recorded in the Result. An error is returned only for setup-time failures
// or context cancellation.
func (r *Runner) Run(ctx context.Context, mirrors []EntryMirror) (res Result, err error) {
	if r.Concurrency <= 0 {
		r.Concurrency = 4
	}
	if r.Retries < 0 {
		r.Retries = 0
	}
	if r.PerJobTimeout <= 0 {
		r.PerJobTimeout = 5 * time.Minute
	}
	if r.Logger == nil {
		r.Logger = slog.Default()
	}

	r.Logger.Info("sync started",
		"entries", len(mirrors),
		"concurrency", r.Concurrency,
		"retries", r.Retries,
		"timeout", r.PerJobTimeout)

	start := time.Now()
	defer func() {
		res.Duration = time.Since(start).Round(time.Millisecond)
	}()

	for _, m := range mirrors {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		entryRes, err := r.runEntry(ctx, m)
		if err != nil {
			// Plan-time error (couldn't list tags etc.) — mark the entry failed
			// but keep going so the rest of the config gets exercised. This is
			// the only path that sets apiv1.EntryFailed; per-tag failures live as
			// apiv1.StatusFailed rows on an apiv1.EntryCompleted entry.
			r.Logger.Error("plan failed", "entry", entryRes.Source, "err", err)
			entryRes.Status = apiv1.EntryFailed
			entryRes.Error = err.Error()
			if r.OnPlanError != nil {
				r.OnPlanError(entryRes.Source, err)
			}
		}
		res.Entries = append(res.Entries, entryRes)
		if r.OnEntryFinished != nil {
			r.OnEntryFinished(entryRes.Source)
		}
	}
	return res, nil
}

func (r *Runner) runEntry(ctx context.Context, m EntryMirror) (apiv1.EntryResult, error) {
	plan, err := m.Plan(ctx)
	if err != nil {
		return apiv1.EntryResult{Source: plan.Source, Destination: plan.Destination, Status: apiv1.EntryFailed, Tags: []apiv1.TagResult{}}, err
	}
	// Pre-size the rows slice so each job writes to its own plan-order index
	// regardless of completion order — output is deterministic even under
	// concurrency. Trimmed below to the number of jobs actually launched.
	out := apiv1.EntryResult{
		Source:      plan.Source,
		Destination: plan.Destination,
		Status:      apiv1.EntryCompleted,
		Tags:        make([]apiv1.TagResult, len(plan.Jobs)),
	}
	r.Logger.Info("entry started", "source", plan.Source, "jobs", len(plan.Jobs))
	defer func(start time.Time) {
		r.Logger.Info("entry finished",
			"source", plan.Source,
			"wall", time.Since(start).Round(time.Millisecond),
			"jobs", len(plan.Jobs),
			"failures", entryFailedCount(out))
	}(time.Now())

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(r.Concurrency)

	var mu sync.Mutex
	record := func(i int, job Job, res JobResult, err error) {
		mu.Lock()
		defer mu.Unlock()
		// Digest and Referrers carry over whatever the job resolved: a
		// copy-failed row keeps the compare digest and any referrers mirrored
		// before the failure; a plan-failed/verify-failed row leaves both empty
		// (zero JobResult), so the error stands alone.
		row := apiv1.TagResult{
			Tag:          job.ID,
			Verification: job.Verification,
			Digest:       res.Digest,
			Referrers:    res.Referrers,
		}
		if err != nil {
			row.Status = apiv1.StatusFailed
			row.Error = err.Error()
		} else {
			row.Status = res.Status
			row.Reason = res.Reason
		}
		out.Tags[i] = row
	}

	launched := 0
	for i, job := range plan.Jobs {
		if gctx.Err() != nil {
			break
		}
		launched++
		g.Go(func() error {
			// A plan-time-known failure is recorded directly, skipping Run and
			// the retry budget — retrying a guaranteed failure is wasted work.
			res, err := JobResult{}, job.PlanError
			if err == nil {
				res, err = r.runJob(gctx, job)
				if err == nil && !res.Status.Valid() {
					err = fmt.Errorf("entry returned invalid status %q", string(res.Status))
				}
			}
			if r.OnJobFinished != nil {
				r.OnJobFinished(plan.Source, job.ID, job.Dst, res.Status, err)
			}
			record(i, job, res, err)
			// Never propagate the error — failures are recorded per-tag and
			// shouldn't cancel sibling tag jobs in the same entry.
			return nil
		})
	}

	werr := g.Wait()
	// Trim to the prefix actually launched (jobs are launched in plan order and
	// the loop breaks on context cancellation), so we never serialize
	// zero-value rows for jobs that never ran.
	out.Tags = out.Tags[:launched]
	if werr != nil {
		return out, werr
	}
	return out, nil
}

// runJob applies the per-tag retry budget. The Run closure may be invoked
// up to Retries+1 times within PerJobTimeout, with exponential backoff
// between attempts. crane.Copy is idempotent so retries are safe.
func (r *Runner) runJob(parent context.Context, job Job) (JobResult, error) {
	ctx, cancel := context.WithTimeout(parent, r.PerJobTimeout)
	defer cancel()

	// Keep the last attempt's result so a copy-failed row still carries the
	// digest/referrers the closure resolved before erroring.
	var lastRes JobResult
	var lastErr error
	for attempt := 0; attempt <= r.Retries; attempt++ {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return lastRes, fmt.Errorf("attempts exhausted (%d) after %w; last: %w",
					attempt, err, lastErr)
			}
			return lastRes, err
		}
		res, err := job.Run(ctx)
		if err == nil {
			return res, nil
		}
		lastRes, lastErr = res, err
		// Don't waste the budget on a retry if the context is already done.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			break
		}
		if attempt < r.Retries {
			backoff := time.Duration(1<<attempt) * 200 * time.Millisecond
			r.Logger.Debug("job failed, retrying",
				"id", job.ID, "attempt", attempt+1, "backoff", backoff, "err", err)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return lastRes, ctx.Err()
			}
		}
	}
	return lastRes, lastErr
}
