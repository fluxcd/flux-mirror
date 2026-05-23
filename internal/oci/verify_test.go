// Copyright 2026 The Flux Authors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"errors"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	sigverify "github.com/sigstore/sigstore-go/pkg/verify"
)

func TestEnforceMinAge(t *testing.T) {
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name      string
		result    *sigverify.VerificationResult
		minAge    time.Duration
		wantErr   string
		wantYoung bool
	}{
		{
			name: "old enough",
			result: &sigverify.VerificationResult{VerifiedTimestamps: []sigverify.TimestampVerificationResult{{
				Type:      "Tlog",
				Timestamp: now.Add(-2 * time.Hour),
			}}},
			minAge: time.Hour,
		},
		{
			name: "uses oldest verified tlog timestamp",
			result: &sigverify.VerificationResult{VerifiedTimestamps: []sigverify.TimestampVerificationResult{
				{Type: "Tlog", Timestamp: now.Add(-30 * time.Minute)},
				{Type: "Tlog", Timestamp: now.Add(-2 * time.Hour)},
			}},
			minAge: time.Hour,
		},
		{
			name: "too new",
			result: &sigverify.VerificationResult{VerifiedTimestamps: []sigverify.TimestampVerificationResult{{
				Type:      "Tlog",
				Timestamp: now.Add(-30 * time.Minute),
			}}},
			minAge:    time.Hour,
			wantErr:   "signature age (30m0s) is less than the required minAge (1h0m0s)",
			wantYoung: true,
		},
		{
			name: "no tlog timestamp",
			result: &sigverify.VerificationResult{VerifiedTimestamps: []sigverify.TimestampVerificationResult{{
				Type:      "TimestampAuthority",
				Timestamp: now.Add(-2 * time.Hour),
			}}},
			minAge:  time.Hour,
			wantErr: "no verified transparency log integrated timestamps",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			err := enforceMinAge(tt.result, tt.minAge, now)
			if tt.wantErr == "" {
				g.Expect(err).ToNot(HaveOccurred())
				return
			}
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(tt.wantErr))
			var young *SignatureTooNewError
			g.Expect(errors.As(err, &young)).To(Equal(tt.wantYoung))
		})
	}
}
