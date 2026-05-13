// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/onsi/gomega"
)

type stubMirror struct {
	name string
	jobs []Job
	err  error
}

func (s *stubMirror) Plan(_ context.Context) (Plan, error) {
	return Plan{Name: s.name, Jobs: s.jobs}, s.err
}

func okJob(tag string, oc Outcome) Job {
	return Job{ID: tag, Run: func(_ context.Context) (Outcome, error) { return oc, nil }}
}

func failJob(tag string) Job {
	return Job{ID: tag, Run: func(_ context.Context) (Outcome, error) {
		return "", errors.New("boom")
	}}
}

func newRunner(t *testing.T) *Runner {
	t.Helper()
	return &Runner{
		Concurrency:   2,
		Retries:       0,
		PerJobTimeout: 2 * time.Second,
	}
}

func TestRunner_AggregatesOutcomes(t *testing.T) {
	g := NewWithT(t)
	r := newRunner(t)

	mirrors := []EntryMirror{
		&stubMirror{name: "entry-a", jobs: []Job{
			okJob("v1", OutcomeCopied),
			okJob("v2", OutcomeSkipped),
		}},
		&stubMirror{name: "entry-b", jobs: []Job{
			okJob("v1", OutcomeOverwritten),
			okJob("v2", OutcomeDrifted),
		}},
	}

	res, err := r.Run(context.Background(), mirrors)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.Entries).To(HaveLen(2))
	g.Expect(res.Entries[0].Outcomes[OutcomeCopied]).To(HaveLen(1))
	g.Expect(res.Entries[0].Outcomes[OutcomeSkipped]).To(HaveLen(1))
	g.Expect(res.Entries[1].Outcomes[OutcomeOverwritten]).To(HaveLen(1))
	g.Expect(res.Entries[1].Outcomes[OutcomeDrifted]).To(Equal([]string{"v2"}))
	g.Expect(res.HasDrift()).To(BeTrue())
	g.Expect(res.HasFailures()).To(BeFalse())
	g.Expect(res.ExitCode()).To(Equal(2))
}

func TestRunner_FailureExitCode(t *testing.T) {
	g := NewWithT(t)
	r := newRunner(t)

	res, err := r.Run(context.Background(), []EntryMirror{
		&stubMirror{name: "x", jobs: []Job{failJob("v1"), okJob("v2", OutcomeCopied)}},
	})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.HasFailures()).To(BeTrue())
	g.Expect(res.ExitCode()).To(Equal(1)) // failures take precedence over anything else
	g.Expect(res.Entries[0].Failures).To(HaveLen(1))
	g.Expect(res.Entries[0].Outcomes[OutcomeCopied]).To(HaveLen(1))
}

func TestRunner_PlanError(t *testing.T) {
	g := NewWithT(t)
	r := newRunner(t)

	res, err := r.Run(context.Background(), []EntryMirror{
		&stubMirror{name: "broken", err: errors.New("list failed")},
	})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.HasFailures()).To(BeTrue())
	g.Expect(res.Entries[0].Failures[0].Tag).To(Equal("<plan>"))
}

func TestRunner_RetriesUntilSuccess(t *testing.T) {
	g := NewWithT(t)
	r := newRunner(t)
	r.Retries = 2

	var attempts atomic.Int32
	job := Job{ID: "v1", Run: func(_ context.Context) (Outcome, error) {
		n := attempts.Add(1)
		if n < 3 {
			return "", errors.New("transient")
		}
		return OutcomeCopied, nil
	}}

	res, err := r.Run(context.Background(), []EntryMirror{
		&stubMirror{name: "retry", jobs: []Job{job}},
	})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(attempts.Load()).To(Equal(int32(3)))
	g.Expect(res.HasFailures()).To(BeFalse())
	g.Expect(res.Entries[0].Outcomes[OutcomeCopied]).To(HaveLen(1))
}

func TestRunner_RetriesExhausted(t *testing.T) {
	g := NewWithT(t)
	r := newRunner(t)
	r.Retries = 2

	var attempts atomic.Int32
	job := Job{ID: "v1", Run: func(_ context.Context) (Outcome, error) {
		attempts.Add(1)
		return "", errors.New("hard")
	}}

	res, err := r.Run(context.Background(), []EntryMirror{
		&stubMirror{name: "exhausted", jobs: []Job{job}},
	})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(attempts.Load()).To(Equal(int32(3))) // retries+1
	g.Expect(res.HasFailures()).To(BeTrue())
}

func TestRunner_TimeoutBudget(t *testing.T) {
	g := NewWithT(t)
	r := newRunner(t)
	r.PerJobTimeout = 100 * time.Millisecond
	r.Retries = 5 // would do many retries if budget allowed

	job := Job{ID: "v1", Run: func(ctx context.Context) (Outcome, error) {
		select {
		case <-time.After(200 * time.Millisecond):
			return OutcomeCopied, nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}}

	start := time.Now()
	res, err := r.Run(context.Background(), []EntryMirror{
		&stubMirror{name: "slow", jobs: []Job{job}},
	})
	elapsed := time.Since(start)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.HasFailures()).To(BeTrue())
	g.Expect(elapsed).To(BeNumerically("<", 500*time.Millisecond),
		"timeout budget should bound total wall time")
}

func TestRunner_ConcurrencyBounded(t *testing.T) {
	g := NewWithT(t)
	r := newRunner(t)
	r.Concurrency = 2

	var inFlight atomic.Int32
	var maxObserved atomic.Int32
	jobs := make([]Job, 10)
	for i := range jobs {
		jobs[i] = Job{ID: fmt.Sprintf("v%d", i), Run: func(_ context.Context) (Outcome, error) {
			n := inFlight.Add(1)
			defer inFlight.Add(-1)
			for {
				cur := maxObserved.Load()
				if n <= cur || maxObserved.CompareAndSwap(cur, n) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			return OutcomeCopied, nil
		}}
	}
	res, err := r.Run(context.Background(), []EntryMirror{
		&stubMirror{name: "fan-out", jobs: jobs},
	})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.Entries[0].Outcomes[OutcomeCopied]).To(HaveLen(10))
	g.Expect(maxObserved.Load()).To(BeNumerically("<=", int32(2)),
		"errgroup.SetLimit should cap concurrent jobs")
}

func TestRunner_EntriesAreSequential(t *testing.T) {
	g := NewWithT(t)
	r := newRunner(t)
	r.Concurrency = 4

	var order []string
	makeJob := func(label string) Job {
		return Job{ID: label, Run: func(_ context.Context) (Outcome, error) {
			time.Sleep(10 * time.Millisecond)
			order = append(order, label)
			return OutcomeCopied, nil
		}}
	}

	mirrors := []EntryMirror{
		&stubMirror{name: "first", jobs: []Job{makeJob("a1"), makeJob("a2")}},
		&stubMirror{name: "second", jobs: []Job{makeJob("b1")}},
	}
	_, err := r.Run(context.Background(), mirrors)
	g.Expect(err).ToNot(HaveOccurred())
	// All 'a*' jobs must complete before 'b1'.
	g.Expect(order[len(order)-1]).To(Equal("b1"))
}

func TestResult_Render(t *testing.T) {
	g := NewWithT(t)
	res := Result{
		Entries: []EntryResult{
			{
				Name: "ghcr.io/foo/bar",
				Outcomes: map[Outcome][]string{
					OutcomeCopied:  {"v1", "v2"},
					OutcomeSkipped: {"v4"},
				},
			},
		},
	}
	res.Entries[0].Outcomes[OutcomeDrifted] = []string{"v3"}

	for _, format := range []string{"yaml", "json"} {
		t.Run(format, func(t *testing.T) {
			g := NewWithT(t)
			var buf bytes.Buffer
			g.Expect(res.Render(&buf, format)).To(Succeed())
			g.Expect(buf.String()).ToNot(BeEmpty())
			g.Expect(buf.String()).To(ContainSubstring("ghcr.io/foo/bar"))
		})
	}

	var buf bytes.Buffer
	g.Expect(res.Render(&buf, "text")).To(MatchError(ContainSubstring("unsupported")))
	g.Expect(res.Render(&buf, "xml")).To(MatchError(ContainSubstring("unsupported")))
}

func TestResult_LogSummary(t *testing.T) {
	g := NewWithT(t)
	res := Result{
		Entries: []EntryResult{
			{
				Name: "ghcr.io/foo/bar",
				Outcomes: map[Outcome][]string{
					OutcomeCopied:  {"v1", "v2"},
					OutcomeSkipped: {"v4"},
					OutcomeDrifted: {"v3"},
				},
			},
			{
				Name: "ghcr.io/foo/baz",
				Outcomes: map[Outcome][]string{
					OutcomeWouldCopy: {"v1", "v2", "v3", "v4"},
				},
				Failures: []TagFailure{{Tag: "v1", Err: "boom"}},
			},
		},
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	res.LogSummary(logger)
	out := buf.String()

	g.Expect(out).To(ContainSubstring(`msg="entry summary"`))
	g.Expect(out).To(ContainSubstring("name=ghcr.io/foo/bar"))
	g.Expect(out).To(ContainSubstring("copied=2"))
	g.Expect(out).To(ContainSubstring("skipped=1"))
	g.Expect(out).To(ContainSubstring("drifted=1"))
	g.Expect(out).To(ContainSubstring("name=ghcr.io/foo/baz"))
	g.Expect(out).To(ContainSubstring("would-copy=4"))
	g.Expect(out).To(ContainSubstring("failed=1"))

	g.Expect(out).To(ContainSubstring(`msg="sync complete"`))
	g.Expect(out).To(ContainSubstring("entries=2"))
}
