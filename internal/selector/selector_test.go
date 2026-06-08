// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package selector

import (
	"testing"

	. "github.com/onsi/gomega"

	apiv1 "github.com/fluxcd/flux-mirror/api/v1beta1"
)

func TestSelect_SemverDefault(t *testing.T) {
	g := NewWithT(t)
	tags := []string{"1.0.0", "2.5.1", "1.9.0", "2.5.0", "not-a-version"}
	sel := apiv1.Selector{Limit: new(2)}

	res, err := Select(tags, sel, Options{})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.Tags).To(Equal([]string{"2.5.1", "2.5.0"}))
}

func TestSelect_SemverConstraint(t *testing.T) {
	g := NewWithT(t)
	tags := []string{"2.40.0", "2.41.0", "3.0.0", "1.99.0"}
	sel := apiv1.Selector{Semver: ">=2.40.0 <3.0.0", Limit: new(5)}

	res, err := Select(tags, sel, Options{})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.Tags).To(Equal([]string{"2.41.0", "2.40.0"}))
}

func TestSelect_LimitUnlimited(t *testing.T) {
	g := NewWithT(t)
	tags := []string{"1.0.0", "1.1.0", "1.2.0", "0.9.0"}
	sel := apiv1.Selector{Limit: new(0)} // 0 = no cap, NOT zero results

	res, err := Select(tags, sel, Options{})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.Tags).To(Equal([]string{"1.2.0", "1.1.0", "1.0.0", "0.9.0"}))
}

func TestSelect_LimitGreaterThanResults(t *testing.T) {
	g := NewWithT(t)
	tags := []string{"1.0.0", "1.1.0"}
	sel := apiv1.Selector{Limit: new(99)}
	res, err := Select(tags, sel, Options{})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.Tags).To(Equal([]string{"1.1.0", "1.0.0"}))
}

func TestSelect_DefaultLimitIsOne(t *testing.T) {
	g := NewWithT(t)
	tags := []string{"1.0.0", "1.1.0", "1.2.0"}
	res, err := Select(tags, apiv1.Selector{}, Options{})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.Tags).To(Equal([]string{"1.2.0"}))
}

func TestSelect_Alphabetical(t *testing.T) {
	g := NewWithT(t)
	tags := []string{
		"RELEASE.2024-11-12T08-30-15Z",
		"RELEASE.2024-12-01T10-00-00Z",
		"RELEASE.2024-10-01T05-00-00Z",
	}
	sel := apiv1.Selector{SortBy: apiv1.SortByAlphabetical, Limit: new(2)}

	res, err := Select(tags, sel, Options{})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.Tags).To(Equal([]string{
		"RELEASE.2024-12-01T10-00-00Z",
		"RELEASE.2024-11-12T08-30-15Z",
	}))
}

func TestSelect_Numerical(t *testing.T) {
	g := NewWithT(t)
	tags := []string{"100", "50", "200", "150"}
	sel := apiv1.Selector{SortBy: apiv1.SortByNumerical, Limit: new(2)}

	res, err := Select(tags, sel, Options{})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.Tags).To(Equal([]string{"200", "150"}))
}

func TestSelect_RegexExtractNumerical(t *testing.T) {
	g := NewWithT(t)
	tags := []string{
		"1.2.3-1731420000",
		"1.2.3-1731500000",
		"1.2.3-1731000000",
		"latest",                // dropped by regex
		"1.2.3",                 // dropped by regex (no -ts suffix)
		"1.2.3-not-a-timestamp", // dropped by regex
	}
	sel := apiv1.Selector{
		Regex:  &apiv1.RegexFilter{Pattern: `^\d+\.\d+\.\d+-(?P<ts>\d+)$`, Extract: "$ts"},
		SortBy: apiv1.SortByNumerical,
		Limit:  new(2),
	}

	res, err := Select(tags, sel, Options{})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.Tags).To(Equal([]string{"1.2.3-1731500000", "1.2.3-1731420000"}))
}

func TestSelect_RegexNoExtract(t *testing.T) {
	g := NewWithT(t)
	tags := []string{"v1.0.0", "v2.0.0", "1.0.0", "rc-1.0.0"}
	sel := apiv1.Selector{
		Regex: &apiv1.RegexFilter{Pattern: `^v\d+\.\d+\.\d+$`},
		Limit: new(5),
	}

	res, err := Select(tags, sel, Options{})
	g.Expect(err).ToNot(HaveOccurred())
	// Default sort is semver — matched "v..." tags must parse as semver.
	g.Expect(res.Tags).To(Equal([]string{"v2.0.0", "v1.0.0"}))
}

func TestSelect_VerboseRecordsExcluded(t *testing.T) {
	g := NewWithT(t)
	tags := []string{"1.0.0", "weird"}
	sel := apiv1.Selector{Limit: new(5)}

	res, err := Select(tags, sel, Options{Verbose: true})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.Tags).To(Equal([]string{"1.0.0"}))
	g.Expect(res.Excluded).To(HaveLen(1))
	g.Expect(res.Excluded[0].Tag).To(Equal("weird"))
	g.Expect(res.Excluded[0].Reason).To(ContainSubstring("not semver"))
}

func TestSelect_EmptyInput(t *testing.T) {
	g := NewWithT(t)
	res, err := Select(nil, apiv1.Selector{Limit: new(5)}, Options{})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.Tags).To(BeEmpty())
}

func TestSelect_AllExcluded(t *testing.T) {
	g := NewWithT(t)
	tags := []string{"a", "b", "c"}
	res, err := Select(tags, apiv1.Selector{Limit: new(5)}, Options{})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.Tags).To(BeEmpty())
}

func TestSelect_RegexAndSemver(t *testing.T) {
	g := NewWithT(t)
	tags := []string{
		"v1.0.0", "v1.5.0", "v2.0.0",
		"1.0.0",   // dropped by regex
		"v0.9.0",  // dropped by semver
		"vlatest", // dropped by regex (matches but not semver)? No, regex requires digits — drops.
	}
	sel := apiv1.Selector{
		Regex:  &apiv1.RegexFilter{Pattern: `^v(?P<v>\d+\.\d+\.\d+)$`, Extract: "$v"},
		Semver: ">=1.0.0",
		Limit:  new(5),
	}
	res, err := Select(tags, sel, Options{})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.Tags).To(Equal([]string{"v2.0.0", "v1.5.0", "v1.0.0"}))
}
