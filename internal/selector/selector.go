// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package selector

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"

	"github.com/Masterminds/semver/v3"

	apiv1 "github.com/fluxcd/flux-mirror/api/v1beta1"
)

// Pipeline:
//
//  1. Optional regex filter: drop tags that do not match. When `extract` is set,
//     the named-capture-group expansion replaces the comparison key while the
//     original tag is preserved for the final output.
//  2. Optional semver constraint filter: drop entries whose comparison key is
//     not a valid semver, then drop entries that fall outside the constraint.
//  3. Sort by the configured strategy. semver/numerical strategies silently
//     drop entries whose key doesn't parse for that strategy (verbose mode
//     records the reason).
//  4. Cap to top-N (highest first). limit == 0 means "no cap".

// Excluded records why a tag was dropped from the pipeline. Only populated
// when Options.Verbose is true.
type Excluded struct {
	Tag    string
	Reason string
}

// Result is the output of Select: the kept tags in selection order plus,
// optionally, the dropped tags with reasons.
type Result struct {
	Tags     []string
	Excluded []Excluded
}

// Options controls Select behavior.
type Options struct {
	Verbose bool
}

// Select runs the full pipeline. The selector is assumed to have already
// passed apiv1.Selector.validate(); errors here indicate programmer error.
func Select(tags []string, sel apiv1.Selector, opts Options) (Result, error) {
	res := Result{}
	record := func(tag, reason string) {
		if opts.Verbose {
			res.Excluded = append(res.Excluded, Excluded{Tag: tag, Reason: reason})
		}
	}

	entries, err := applyRegex(tags, sel.Regex, record)
	if err != nil {
		return Result{}, err
	}

	entries, err = applySemverConstraint(entries, sel.Semver, record)
	if err != nil {
		return Result{}, err
	}

	switch sel.EffectiveSortBy() {
	case apiv1.SortBySemver:
		entries = sortSemver(entries, record)
	case apiv1.SortByAlphabetical:
		entries = sortAlphabetical(entries)
	case apiv1.SortByNumerical:
		entries = sortNumerical(entries, record)
	default:
		return Result{}, fmt.Errorf("unknown sortBy %q (config validation should have caught this)", sel.EffectiveSortBy())
	}

	limit := sel.EffectiveLimit()
	if limit > 0 && limit < len(entries) {
		entries = entries[:limit]
	}

	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.tag
	}
	res.Tags = out
	return res, nil
}

type entry struct {
	key string // comparison key (raw tag or regex extraction)
	tag string // original tag
}

func applyRegex(tags []string, rf *apiv1.RegexFilter, record func(tag, reason string)) ([]entry, error) {
	if rf == nil {
		out := make([]entry, len(tags))
		for i, t := range tags {
			out[i] = entry{key: t, tag: t}
		}
		return out, nil
	}
	re, err := regexp.Compile(rf.Pattern)
	if err != nil {
		return nil, fmt.Errorf("compile regex %q: %w", rf.Pattern, err)
	}
	out := make([]entry, 0, len(tags))
	for _, t := range tags {
		idx := re.FindStringSubmatchIndex(t)
		if idx == nil {
			record(t, "did not match regex")
			continue
		}
		key := t
		if rf.Extract != "" {
			var b []byte
			b = re.ExpandString(b, rf.Extract, t, idx)
			key = string(b)
		}
		out = append(out, entry{key: key, tag: t})
	}
	return out, nil
}

func applySemverConstraint(entries []entry, constraint string, record func(tag, reason string)) ([]entry, error) {
	if constraint == "" {
		return entries, nil
	}
	c, err := semver.NewConstraint(constraint)
	if err != nil {
		return nil, fmt.Errorf("parse semver constraint %q: %w", constraint, err)
	}
	out := make([]entry, 0, len(entries))
	for _, e := range entries {
		v, err := semver.NewVersion(e.key)
		if err != nil {
			record(e.tag, "key not semver: "+e.key)
			continue
		}
		if !c.Check(v) {
			record(e.tag, "fails semver constraint: "+e.key)
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

func sortSemver(entries []entry, record func(tag, reason string)) []entry {
	type sv struct {
		e entry
		v *semver.Version
	}
	parsed := make([]sv, 0, len(entries))
	for _, e := range entries {
		v, err := semver.NewVersion(e.key)
		if err != nil {
			record(e.tag, "not semver for sort: "+e.key)
			continue
		}
		parsed = append(parsed, sv{e: e, v: v})
	}
	sort.SliceStable(parsed, func(i, j int) bool { return parsed[i].v.GreaterThan(parsed[j].v) })
	out := make([]entry, len(parsed))
	for i, p := range parsed {
		out[i] = p.e
	}
	return out
}

func sortAlphabetical(entries []entry) []entry {
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].key > entries[j].key })
	return entries
}

func sortNumerical(entries []entry, record func(tag, reason string)) []entry {
	type nv struct {
		e entry
		v float64
	}
	parsed := make([]nv, 0, len(entries))
	for _, e := range entries {
		v, err := strconv.ParseFloat(e.key, 64)
		if err != nil {
			record(e.tag, "not numeric for sort: "+e.key)
			continue
		}
		parsed = append(parsed, nv{e: e, v: v})
	}
	sort.SliceStable(parsed, func(i, j int) bool { return parsed[i].v > parsed[j].v })
	out := make([]entry, len(parsed))
	for i, p := range parsed {
		out[i] = p.e
	}
	return out
}
