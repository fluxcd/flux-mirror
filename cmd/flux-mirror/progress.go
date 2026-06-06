// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/briandowns/spinner"

	syncpkg "github.com/fluxcd/flux-mirror/internal/sync"
)

// progress drives a single global spinner (on stderr) plus per-job
// completion lines (on stdout) for the human-friendly text mode. It is
// intentionally only wired in non-verbose runs — the verbose log stream
// already conveys per-tag progress and a spinner would just compete with it.
//
// When ttyOut is nil (the --no-progress path) the spinner is omitted but
// per-job lines and PlanFailed lines still print. That mode is for CI logs
// where ANSI cursor escapes from the spinner are noise.
//
// The "N/M done" counter ticks on entry completion, not on individual
// jobs, so the denominator is fixed at construction (= number of entries
// in the config) and the user sees stable, predictable progression.
type progress struct {
	mu           sync.Mutex
	spinner      *spinner.Spinner // nil when --no-progress is set
	out          io.Writer
	totalEntries int
	entriesDone  int
}

func newProgress(out, ttyOut io.Writer, totalEntries int) *progress {
	p := &progress{out: out, totalEntries: totalEntries}
	if ttyOut != nil {
		p.spinner = spinner.New(spinner.CharSets[14], 100*time.Millisecond, spinner.WithWriter(ttyOut))
		p.refreshSuffixLocked()
		p.spinner.Start()
	}
	return p
}

// JobFinished is the OnJobFinished hook for sync.Runner. Safe for
// concurrent invocation.
func (p *progress) JobFinished(entry, id, dst string, st syncpkg.Status, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		p.emitLocked("✗ %s:%s — %v\n", entry, id, err)
		if dst != "" {
			p.emitLocked("→ %s\n", dst)
		}
		return
	}
	p.emitLocked("✓ %s:%s (%s)\n", entry, id, st)
	if dst != "" {
		p.emitLocked("→ %s\n", dst)
	}
}

// EntryFinished is the OnEntryFinished hook for sync.Runner. Called once
// per entry from the main goroutine; advances the spinner counter.
func (p *progress) EntryFinished(_ string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.entriesDone++
	p.refreshSuffixLocked()
}

// PlanFailed is the OnPlanError hook for sync.Runner. Without this,
// plan-time failures (ListTags rejected, auth errors, etc.) would only
// surface in the final Summary count — never as actionable per-entry
// detail.
func (p *progress) PlanFailed(entry string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.emitLocked("✗ %s — plan failed: %v\n", entry, err)
}

func (p *progress) refreshSuffixLocked() {
	if p.spinner == nil {
		return
	}
	p.spinner.Suffix = fmt.Sprintf(" syncing... (%d/%d done)", p.entriesDone, p.totalEntries)
}

// emitLocked stops the spinner (if any), writes one line, then restarts
// it so the per-tag line lands cleanly above the spinner. Write errors on
// a TTY printer are not actionable here, so they're swallowed.
func (p *progress) emitLocked(format string, args ...any) {
	if p.spinner != nil {
		p.spinner.Stop()
	}
	_, _ = fmt.Fprintf(p.out, format, args...)
	if p.spinner != nil {
		p.spinner.Start()
	}
}

// Close stops the spinner. Idempotent — spinner.Stop is a no-op once
// stopped, and a no-op altogether when the spinner was never created.
func (p *progress) Close() {
	if p.spinner != nil {
		p.spinner.Stop()
	}
}
