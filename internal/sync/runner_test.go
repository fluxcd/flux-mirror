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
	return Plan{Source: s.name, Destination: s.name + "/dst", Jobs: s.jobs}, s.err
}

func okJob(tag string, st Status) Job {
	return Job{ID: tag, Run: func(_ context.Context) (JobResult, error) {
		return JobResult{Status: st}, nil
	}}
}

func failJob(tag string) Job {
	return Job{ID: tag, Run: func(_ context.Context) (JobResult, error) {
		return JobResult{}, errors.New("boom")
	}}
}

// rowByTag returns the tag row with the given ID, for order-independent
// assertions.
func rowByTag(e EntryResult, tag string) (TagResult, bool) {
	for _, t := range e.Tags {
		if t.Tag == tag {
			return t, true
		}
	}
	return TagResult{}, false
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
			okJob("v1", StatusCopied),
			okJob("v2", StatusSkipped),
		}},
		&stubMirror{name: "entry-b", jobs: []Job{
			okJob("v1", StatusOverwritten),
			okJob("v2", StatusDrifted),
		}},
	}

	res, err := r.Run(context.Background(), mirrors)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.Entries).To(HaveLen(2))

	// Rows are emitted in plan order regardless of completion order.
	g.Expect(res.Entries[0].Status).To(Equal(EntryCompleted))
	g.Expect(res.Entries[0].Tags).To(HaveLen(2))
	g.Expect(res.Entries[0].Tags[0].Tag).To(Equal("v1"))
	g.Expect(res.Entries[0].Tags[0].Status).To(Equal(StatusCopied))
	g.Expect(res.Entries[0].Tags[1].Tag).To(Equal("v2"))
	g.Expect(res.Entries[0].Tags[1].Status).To(Equal(StatusSkipped))

	g.Expect(res.Entries[1].Tags[0].Status).To(Equal(StatusOverwritten))
	g.Expect(res.Entries[1].Tags[1].Tag).To(Equal("v2"))
	g.Expect(res.Entries[1].Tags[1].Status).To(Equal(StatusDrifted))

	g.Expect(res.HasDrift()).To(BeTrue())
	g.Expect(res.HasFailures()).To(BeFalse())
	g.Expect(res.ExitCode()).To(Equal(2))
}

func TestRunner_FailureExitCode(t *testing.T) {
	g := NewWithT(t)
	r := newRunner(t)

	res, err := r.Run(context.Background(), []EntryMirror{
		&stubMirror{name: "x", jobs: []Job{failJob("v1"), okJob("v2", StatusCopied)}},
	})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.HasFailures()).To(BeTrue())
	g.Expect(res.ExitCode()).To(Equal(1)) // failures take precedence over anything else
	g.Expect(res.TotalFailures()).To(Equal(1))

	// A per-tag failure is a StatusFailed row on a still-completed entry.
	g.Expect(res.Entries[0].Status).To(Equal(EntryCompleted))
	failed, ok := rowByTag(res.Entries[0], "v1")
	g.Expect(ok).To(BeTrue())
	g.Expect(failed.Status).To(Equal(StatusFailed))
	g.Expect(failed.Error).To(ContainSubstring("boom"))
	copied, ok := rowByTag(res.Entries[0], "v2")
	g.Expect(ok).To(BeTrue())
	g.Expect(copied.Status).To(Equal(StatusCopied))
}

func TestRunner_PlanError(t *testing.T) {
	g := NewWithT(t)
	r := newRunner(t)

	res, err := r.Run(context.Background(), []EntryMirror{
		&stubMirror{name: "broken", err: errors.New("list failed")},
	})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.HasFailures()).To(BeTrue())
	g.Expect(res.TotalFailures()).To(Equal(1))

	// A plan-time error marks the entry itself failed (no <plan> pseudo-tag).
	g.Expect(res.Entries[0].Status).To(Equal(EntryFailed))
	g.Expect(res.Entries[0].Error).To(ContainSubstring("list failed"))
	g.Expect(res.Entries[0].Tags).To(BeEmpty())
}

func TestRunner_RetriesUntilSuccess(t *testing.T) {
	g := NewWithT(t)
	r := newRunner(t)
	r.Retries = 2

	var attempts atomic.Int32
	job := Job{ID: "v1", Run: func(_ context.Context) (JobResult, error) {
		n := attempts.Add(1)
		if n < 3 {
			return JobResult{}, errors.New("transient")
		}
		return JobResult{Status: StatusCopied}, nil
	}}

	res, err := r.Run(context.Background(), []EntryMirror{
		&stubMirror{name: "retry", jobs: []Job{job}},
	})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(attempts.Load()).To(Equal(int32(3)))
	g.Expect(res.HasFailures()).To(BeFalse())
	g.Expect(res.Entries[0].Tags[0].Status).To(Equal(StatusCopied))
}

func TestRunner_RetriesExhausted(t *testing.T) {
	g := NewWithT(t)
	r := newRunner(t)
	r.Retries = 2

	var attempts atomic.Int32
	job := Job{ID: "v1", Run: func(_ context.Context) (JobResult, error) {
		attempts.Add(1)
		return JobResult{}, errors.New("hard")
	}}

	res, err := r.Run(context.Background(), []EntryMirror{
		&stubMirror{name: "exhausted", jobs: []Job{job}},
	})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(attempts.Load()).To(Equal(int32(3))) // retries+1
	g.Expect(res.HasFailures()).To(BeTrue())
	g.Expect(res.Entries[0].Tags[0].Status).To(Equal(StatusFailed))
}

func TestRunner_TimeoutBudget(t *testing.T) {
	g := NewWithT(t)
	r := newRunner(t)
	r.PerJobTimeout = 100 * time.Millisecond
	r.Retries = 5 // would do many retries if budget allowed

	job := Job{ID: "v1", Run: func(ctx context.Context) (JobResult, error) {
		select {
		case <-time.After(200 * time.Millisecond):
			return JobResult{Status: StatusCopied}, nil
		case <-ctx.Done():
			return JobResult{}, ctx.Err()
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
		jobs[i] = Job{ID: fmt.Sprintf("v%d", i), Run: func(_ context.Context) (JobResult, error) {
			n := inFlight.Add(1)
			defer inFlight.Add(-1)
			for {
				cur := maxObserved.Load()
				if n <= cur || maxObserved.CompareAndSwap(cur, n) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			return JobResult{Status: StatusCopied}, nil
		}}
	}
	res, err := r.Run(context.Background(), []EntryMirror{
		&stubMirror{name: "fan-out", jobs: jobs},
	})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.Entries[0].Tags).To(HaveLen(10))
	for _, row := range res.Entries[0].Tags {
		g.Expect(row.Status).To(Equal(StatusCopied))
	}
	g.Expect(maxObserved.Load()).To(BeNumerically("<=", int32(2)),
		"errgroup.SetLimit should cap concurrent jobs")
}

func TestRunner_EntriesAreSequential(t *testing.T) {
	g := NewWithT(t)
	r := newRunner(t)
	r.Concurrency = 4

	var order []string
	makeJob := func(label string) Job {
		return Job{ID: label, Run: func(_ context.Context) (JobResult, error) {
			time.Sleep(10 * time.Millisecond)
			order = append(order, label)
			return JobResult{Status: StatusCopied}, nil
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

func TestReport_Render(t *testing.T) {
	g := NewWithT(t)
	res := Result{
		Entries: []EntryResult{
			{
				Source:      "ghcr.io/foo/bar",
				Destination: "localhost:5050/bar",
				Status:      EntryCompleted,
				Tags: []TagResult{
					{
						Tag:    "v1",
						Status: StatusCopied,
						Digest: "sha256:1f5d3c8a9b2e47f6a0c1d8e7b4a59362f0e1d2c3b4a5968778695a4b3c2d1e0f",
						Verification: &Verification{
							Provider:       "cosign",
							Issuer:         "https://token.actions.githubusercontent.com",
							Identity:       "https://github.com/foo/bar/.github/workflows/release.yml@refs/tags/v1",
							IntegratedTime: "2026-05-20T09:14:02Z",
						},
					},
					{Tag: "v2", Status: StatusCopied},
					{Tag: "v4", Status: StatusSkipped, Reason: ReasonUpToDate},
					{Tag: "v3", Status: StatusDrifted},
				},
			},
		},
		Duration: 1500 * time.Millisecond,
	}

	ts := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	report := NewReport("flux-mirror/v1.2.3", ts, res)

	// Envelope and summary are assembled from the Result.
	g.Expect(report.Version).To(Equal(ReportVersion))
	g.Expect(report.Schema).To(Equal(ReportSchema))
	g.Expect(report.Report.Reporter).To(Equal("flux-mirror/v1.2.3"))
	g.Expect(report.Report.Timestamp).To(Equal("2026-06-06T12:00:00Z"))
	g.Expect(report.Report.DurationMs).To(Equal(int64(1500)))
	g.Expect(report.Report.Summary).To(Equal(ReportSummary{
		Entries: 1,
		Copied:  2,
		Skipped: 1,
		Drifted: 1,
		// drift with no failures classifies as exit code 2.
		ExitCode: 2,
	}))

	for _, format := range []string{"yaml", "json"} {
		t.Run(format, func(t *testing.T) {
			g := NewWithT(t)
			var buf bytes.Buffer
			g.Expect(report.Render(&buf, format)).To(Succeed())
			g.Expect(buf.String()).ToNot(BeEmpty())
			g.Expect(buf.String()).To(ContainSubstring("ghcr.io/foo/bar"))
			g.Expect(buf.String()).To(ContainSubstring("flux-mirror/v1.2.3"))
			// Verification metadata is rendered on the row.
			g.Expect(buf.String()).To(ContainSubstring("token.actions.githubusercontent.com"))
		})
	}

	// $schema is JSON-only; YAML consumers don't use it.
	var jsonBuf, yamlBuf bytes.Buffer
	g.Expect(report.Render(&jsonBuf, "json")).To(Succeed())
	g.Expect(jsonBuf.String()).To(ContainSubstring(`"$schema"`))
	g.Expect(report.Render(&yamlBuf, "yaml")).To(Succeed())
	g.Expect(yamlBuf.String()).ToNot(ContainSubstring("$schema"))

	var buf bytes.Buffer
	g.Expect(report.Render(&buf, "text")).To(MatchError(ContainSubstring("unsupported")))
	g.Expect(report.Render(&buf, "xml")).To(MatchError(ContainSubstring("unsupported")))
}

// NewReport must emit results as an empty array, never null, so consumers can
// always range over report.results.
func TestReport_EmptyResults(t *testing.T) {
	g := NewWithT(t)
	report := NewReport("flux-mirror/v1.2.3", time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC), Result{})
	var buf bytes.Buffer
	g.Expect(report.Render(&buf, "json")).To(Succeed())
	g.Expect(buf.String()).To(ContainSubstring(`"results": []`))
}

func TestResult_LogSummary(t *testing.T) {
	g := NewWithT(t)
	res := Result{
		Entries: []EntryResult{
			{
				Source:      "ghcr.io/foo/bar",
				Destination: "localhost:5050/bar",
				Status:      EntryCompleted,
				Tags: []TagResult{
					{Tag: "v1", Status: StatusCopied},
					{Tag: "v2", Status: StatusCopied},
					{Tag: "v4", Status: StatusSkipped, Reason: ReasonUpToDate},
					{Tag: "v3", Status: StatusDrifted},
				},
			},
			{
				Source:      "ghcr.io/foo/baz",
				Destination: "localhost:5050/baz",
				Status:      EntryCompleted,
				Tags: []TagResult{
					{Tag: "v1", Status: StatusWouldCopy},
					{Tag: "v2", Status: StatusWouldCopy},
					{Tag: "v3", Status: StatusWouldCopy},
					{Tag: "v4", Status: StatusWouldCopy},
					{Tag: "v5", Status: StatusFailed, Error: "boom"},
				},
			},
		},
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	res.LogSummary(logger)
	out := buf.String()

	g.Expect(out).To(ContainSubstring(`msg="entry summary"`))
	g.Expect(out).To(ContainSubstring("source=ghcr.io/foo/bar"))
	g.Expect(out).To(ContainSubstring("copied=2"))
	g.Expect(out).To(ContainSubstring("skipped=1"))
	g.Expect(out).To(ContainSubstring("drifted=1"))
	g.Expect(out).To(ContainSubstring("source=ghcr.io/foo/baz"))
	g.Expect(out).To(ContainSubstring("would-copy=4"))
	g.Expect(out).To(ContainSubstring("failed=1"))

	g.Expect(out).To(ContainSubstring(`msg="sync complete"`))
	g.Expect(out).To(ContainSubstring("entries=2"))
}
