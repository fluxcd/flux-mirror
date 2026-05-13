// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package sync

import (
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
}

// Run executes all mirrors and returns a Result aggregating their outcomes.
// It does not return an error for ordinary mirror failures — those are
// recorded in the Result. An error is returned only for setup-time failures
// or context cancellation.
func (r *Runner) Run(ctx context.Context, mirrors []EntryMirror) (Result, error) {
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

	res := Result{}
	for _, m := range mirrors {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		entryRes, err := r.runEntry(ctx, m)
		if err != nil {
			// Plan-time error (couldn't list tags etc.) — record as a single
			// entry-level failure but keep going so the rest of the config
			// gets exercised.
			r.Logger.Error("plan failed", "entry", entryRes.Name, "err", err)
			entryRes.Failures = append(entryRes.Failures, TagFailure{
				Tag: "<plan>", Err: err.Error(),
			})
		}
		res.Entries = append(res.Entries, entryRes)
	}
	return res, nil
}

func (r *Runner) runEntry(ctx context.Context, m EntryMirror) (EntryResult, error) {
	plan, err := m.Plan(ctx)
	if err != nil {
		return EntryResult{Name: plan.Name, Outcomes: map[Outcome]int{}}, err
	}
	out := EntryResult{
		Name:     plan.Name,
		Outcomes: map[Outcome]int{},
	}
	r.Logger.Info("entry started", "name", plan.Name, "jobs", len(plan.Jobs))
	defer func(start time.Time) {
		r.Logger.Info("entry finished",
			"name", plan.Name,
			"wall", time.Since(start).Round(time.Millisecond),
			"jobs", len(plan.Jobs),
			"failures", len(out.Failures))
	}(time.Now())

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(r.Concurrency)

	var mu sync.Mutex
	record := func(tag string, oc Outcome, err error) {
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			out.Failures = append(out.Failures, TagFailure{Tag: tag, Err: err.Error()})
			return
		}
		out.Outcomes[oc]++
		if oc == OutcomeDrifted {
			out.Drifted = append(out.Drifted, tag)
		}
	}

	for _, job := range plan.Jobs {
		if gctx.Err() != nil {
			break
		}
		g.Go(func() error {
			oc, err := r.runJob(gctx, job)
			if err == nil && !oc.Valid() {
				err = fmt.Errorf("entry returned invalid outcome %q", string(oc))
			}
			record(job.ID, oc, err)
			// Never propagate the error — failures are recorded per-tag and
			// shouldn't cancel sibling tag jobs in the same entry.
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return out, err
	}
	return out, nil
}

// runJob applies the per-tag retry budget. The Run closure may be invoked
// up to Retries+1 times within PerJobTimeout, with exponential backoff
// between attempts. crane.Copy is idempotent so retries are safe.
func (r *Runner) runJob(parent context.Context, job Job) (Outcome, error) {
	ctx, cancel := context.WithTimeout(parent, r.PerJobTimeout)
	defer cancel()

	var lastErr error
	for attempt := 0; attempt <= r.Retries; attempt++ {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return "", fmt.Errorf("attempts exhausted (%d) after %w; last: %w",
					attempt, err, lastErr)
			}
			return "", err
		}
		oc, err := job.Run(ctx)
		if err == nil {
			return oc, nil
		}
		lastErr = err
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
				return "", ctx.Err()
			}
		}
	}
	return "", lastErr
}
