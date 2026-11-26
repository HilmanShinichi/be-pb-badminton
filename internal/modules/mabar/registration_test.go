package mabar

import (
	"testing"
	"time"
)

func TestComputeRegistrationQuotas(t *testing.T) {
	opens := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	memberCloses := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	closes := time.Date(2026, 11, 28, 3, 0, 0, 0, time.UTC)

	t.Run("before member cutoff both member and non-member can join their own quotas", func(t *testing.T) {
		now := time.Date(2026, 11, 26, 14, 0, 0, 0, time.UTC)
		status := computeRegistrationStatus(&opens, &memberCloses, &closes, now)
		if status != "MEMBER_ONLY" {
			t.Fatalf("expected MEMBER_ONLY, got %s", status)
		}
		if isMemberDeadlinePassed(&memberCloses, now) {
			t.Fatalf("expected member deadline not passed")
		}

		q := computeRegistrationQuotas(20, 8, 3, 15, 3, 0, status, &memberCloses, now)
		if q.BonusNonMemberSlots != 0 {
			t.Fatalf("expected 0 bonus slots before cutoff, got %d", q.BonusNonMemberSlots)
		}
		if q.EffectiveMaxNonMembers != 8 {
			t.Fatalf("expected effective max non-members 8, got %d", q.EffectiveMaxNonMembers)
		}
		if q.AvailableMemberSlots != 5 {
			t.Fatalf("expected 5 available member slots, got %d", q.AvailableMemberSlots)
		}
		if q.AvailableNonMemberSlots != 5 {
			t.Fatalf("expected 5 available non-member slots, got %d", q.AvailableNonMemberSlots)
		}
	})

	t.Run("after member cutoff unused member slots transfer to non-members and member closes", func(t *testing.T) {
		now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
		status := computeRegistrationStatus(&opens, &memberCloses, &closes, now)
		if status != "OPEN" {
			t.Fatalf("expected OPEN, got %s", status)
		}
		if !isMemberDeadlinePassed(&memberCloses, now) {
			t.Fatalf("expected member deadline passed")
		}

		q := computeRegistrationQuotas(20, 8, 3, 15, 8, 0, status, &memberCloses, now)
		if q.AvailableMemberSlots != 0 {
			t.Fatalf("expected 0 available member slots after cutoff, got %d", q.AvailableMemberSlots)
		}
		if q.BonusNonMemberSlots != 5 {
			t.Fatalf("expected 5 bonus slots (20-15), got %d", q.BonusNonMemberSlots)
		}
		if q.EffectiveMaxNonMembers != 13 {
			t.Fatalf("expected effective max non-members 13 (8+5), got %d", q.EffectiveMaxNonMembers)
		}
		if q.AvailableNonMemberSlots != 5 {
			t.Fatalf("expected 5 available non-member slots (13-8), got %d", q.AvailableNonMemberSlots)
		}
	})

	t.Run("after member cutoff overfilled members does not reduce non-member base quota", func(t *testing.T) {
		now := time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC)
		status := computeRegistrationStatus(&opens, &memberCloses, &closes, now)
		q := computeRegistrationQuotas(20, 8, 3, 22, 5, 0, status, &memberCloses, now)
		if q.BonusNonMemberSlots != 0 {
			t.Fatalf("expected 0 bonus slots, got %d", q.BonusNonMemberSlots)
		}
		if q.EffectiveMaxNonMembers != 8 {
			t.Fatalf("expected effective max non-members 8, got %d", q.EffectiveMaxNonMembers)
		}
		if q.AvailableNonMemberSlots != 3 {
			t.Fatalf("expected 3 available non-member slots, got %d", q.AvailableNonMemberSlots)
		}
	})

	t.Run("after registration closes all available slots are zero", func(t *testing.T) {
		now := time.Date(2026, 11, 28, 4, 0, 0, 0, time.UTC)
		status := computeRegistrationStatus(&opens, &memberCloses, &closes, now)
		if status != "CLOSED" {
			t.Fatalf("expected CLOSED, got %s", status)
		}
		q := computeRegistrationQuotas(20, 8, 3, 10, 4, 1, status, &memberCloses, now)
		if q.AvailableMemberSlots != 0 || q.AvailableNonMemberSlots != 0 || q.AvailableWaitingSlots != 0 {
			t.Fatalf("expected all available slots 0 when closed, got %+v", q)
		}
	})

	t.Run("without member cutoff both remain open until close", func(t *testing.T) {
		now := time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC)
		status := computeRegistrationStatus(&opens, nil, &closes, now)
		if status != "OPEN" {
			t.Fatalf("expected OPEN, got %s", status)
		}
		q := computeRegistrationQuotas(20, 8, 3, 15, 3, 0, status, nil, now)
		if q.AvailableMemberSlots != 5 || q.AvailableNonMemberSlots != 5 {
			t.Fatalf("expected 5 member and 5 non-member slots, got %+v", q)
		}
	})
}
