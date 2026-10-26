package main

import (
	"testing"
	"time"
)

func TestDuplicateSubmissionErrorNilReceiverAccessors(t *testing.T) {
	t.Parallel()

	var err *duplicateSubmissionError
	if got := err.DuplicateRequestID(); got != "" {
		t.Fatalf("DuplicateRequestID(nil) = %q, want empty", got)
	}
	if got := err.DuplicateState(); got != "" {
		t.Fatalf("DuplicateState(nil) = %q, want empty", got)
	}
}

func TestNewLocalSubmissionDeduperDefaultsNonPositiveTTL(t *testing.T) {
	t.Parallel()

	for _, ttl := range []time.Duration{0, -time.Minute} {
		d := newLocalSubmissionDeduper(ttl)
		if d.ttl != 30*time.Minute {
			t.Fatalf("newLocalSubmissionDeduper(%v).ttl = %v, want 30m default", ttl, d.ttl)
		}
	}
}

func TestLocalSubmissionDeduperNilReceiverMethodsAreSafe(t *testing.T) {
	t.Parallel()

	var d *localSubmissionDeduper

	if duplicate, state, dup := d.Check("key", false); duplicate || state != "" || dup != "" {
		t.Fatalf("Check(nil) = (%v, %q, %q), want (false, \"\", \"\")", duplicate, state, dup)
	}
	if accepted, state, dup := d.Begin("key", "req", false); !accepted || state != "accepted" || dup != "" {
		t.Fatalf("Begin(nil) = (%v, %q, %q), want (true, accepted, \"\")", accepted, state, dup)
	}
	d.Done("key", "req", "ok") // should not panic
}

func TestLocalSubmissionDeduperCheckAndBeginEmptyKeyAreNoops(t *testing.T) {
	t.Parallel()

	d := newLocalSubmissionDeduper(time.Minute)

	if duplicate, state, dup := d.Check("   ", false); duplicate || state != "" || dup != "" {
		t.Fatalf("Check(empty key) = (%v, %q, %q), want (false, \"\", \"\")", duplicate, state, dup)
	}
	if accepted, state, dup := d.Begin("   ", "req", false); !accepted || state != "accepted" || dup != "" {
		t.Fatalf("Begin(empty key) = (%v, %q, %q), want (true, accepted, \"\")", accepted, state, dup)
	}
	d.Done("   ", "req", "ok") // should not panic / mutate state
}
