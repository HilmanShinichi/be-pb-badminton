package period

import (
	"testing"
	"time"
)

func strPtr(s string) *string { return &s }

func TestNormalizeAndCoalesceOptionalText(t *testing.T) {
	if got := normalizeOptionalText(nil); got != nil {
		t.Fatalf("expected nil for nil input, got %q", *got)
	}
	if got := normalizeOptionalText(strPtr("   \n\t  ")); got != nil {
		t.Fatalf("expected nil for whitespace input, got %q", *got)
	}
	got := normalizeOptionalText(strPtr("  Open member 5x pertemuan\nLapangan 1  "))
	if got == nil || *got != "Open member 5x pertemuan\nLapangan 1" {
		t.Fatalf("unexpected trimmed value: %v", got)
	}

	existing := strPtr("Ketentuan lama")
	if kept := coalesceOptionalText(nil, existing); kept == nil || *kept != "Ketentuan lama" {
		t.Fatalf("expected nil patch to preserve existing notes, got %v", kept)
	}
	if cleared := coalesceOptionalText(strPtr("   "), existing); cleared != nil {
		t.Fatalf("expected blank patch to clear existing notes, got %q", *cleared)
	}
	if updated := coalesceOptionalText(strPtr(" Ketentuan baru "), existing); updated == nil || *updated != "Ketentuan baru" {
		t.Fatalf("expected trimmed update, got %v", updated)
	}
}

func intPtr(v int) *int { return &v }

func TestEvaluateQuotaForApproval(t *testing.T) {
	can, fills := evaluateQuotaForApproval(nil, 25)
	if !can || fills {
		t.Fatalf("expected unlimited quota to allow without filling last slot, got can=%v fills=%v", can, fills)
	}

	can, fills = evaluateQuotaForApproval(intPtr(0), 10)
	if !can || fills {
		t.Fatalf("expected zero maxMembers to behave as unlimited, got can=%v fills=%v", can, fills)
	}

	can, fills = evaluateQuotaForApproval(intPtr(20), 18)
	if !can || fills {
		t.Fatalf("expected 18/20 to allow without filling last slot, got can=%v fills=%v", can, fills)
	}

	can, fills = evaluateQuotaForApproval(intPtr(20), 19)
	if !can || !fills {
		t.Fatalf("expected 19/20 to allow and fill last slot, got can=%v fills=%v", can, fills)
	}

	can, fills = evaluateQuotaForApproval(intPtr(20), 20)
	if can || fills {
		t.Fatalf("expected 20/20 to block approval, got can=%v fills=%v", can, fills)
	}
}

func TestParseAndCoalesceOptionalUTCTimestamp(t *testing.T) {
	if got, err := parseOptionalUTCTimestamp(nil); err != nil || got != nil {
		t.Fatalf("expected (nil, nil), got (%v, %v)", got, err)
	}
	if got, err := parseOptionalUTCTimestamp(strPtr("   ")); err != nil || got != nil {
		t.Fatalf("expected (nil, nil) for blank, got (%v, %v)", got, err)
	}
	if _, err := parseOptionalUTCTimestamp(strPtr("2026-12-06 18:00")); err == nil {
		t.Fatalf("expected error for non-ISO timestamp")
	}

	// GMT+7 input should normalize cleanly to UTC-7 hours
	got, err := parseOptionalUTCTimestamp(strPtr("2026-12-06T23:59:00+07:00"))
	if err != nil || got == nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Location() != time.UTC || got.Format(time.RFC3339) != "2026-12-06T16:59:00Z" {
		t.Fatalf("expected 2026-12-06T16:59:00Z, got %s (%s)", got.Format(time.RFC3339), got.Location())
	}

	existing := strPtr("2026-12-06T16:59:00Z")
	if kept, err := coalesceOptionalUTCTimestamp(nil, existing); err != nil || kept == nil || kept.Format(time.RFC3339) != "2026-12-06T16:59:00Z" {
		t.Fatalf("expected nil patch to preserve existing deadline, got %v %v", kept, err)
	}
	if cleared, err := coalesceOptionalUTCTimestamp(strPtr(""), existing); err != nil || cleared != nil {
		t.Fatalf("expected empty patch to clear existing deadline, got %v %v", cleared, err)
	}
}

func TestIsRegistrationDeadlinePassed(t *testing.T) {
	now := time.Date(2026, 12, 6, 17, 0, 0, 0, time.UTC) // 2026-12-07 00:00 GMT+7

	if isRegistrationDeadlinePassed(nil, now) {
		t.Fatalf("nil deadline must not be treated as passed")
	}
	if isRegistrationDeadlinePassed(strPtr("2026-12-06T17:00:01Z"), now) {
		t.Fatalf("future deadline must not be treated as passed")
	}
	if !isRegistrationDeadlinePassed(strPtr("2026-12-06T17:00:00Z"), now) {
		t.Fatalf("exact boundary deadline must be treated as passed")
	}
	if !isRegistrationDeadlinePassed(strPtr("2026-12-06T23:59:00+07:00"), now) {
		t.Fatalf("past GMT+7 deadline must be treated as passed")
	}
}
