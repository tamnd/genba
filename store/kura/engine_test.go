//go:build cgo && kura

package kura

import "testing"

// TestTheStatusCodesMeanWhatWeSayTheyMean is why the codes are mirrored in Go
// rather than read through cgo. A caller in either build gets the same error
// value for the same condition, and this is the check that the copy has not
// drifted from the engine it was copied from.
func TestTheStatusCodesMeanWhatWeSayTheyMean(t *testing.T) {
	for code, want := range statusMessages {
		got := statusMessage(code)
		if got != want {
			t.Errorf("status %d is %q in the engine and %q here", code, got, want)
		}
	}
	// A code the engine does not know must still produce something rather than
	// a null pointer to dereference.
	if s := Status(9999); s.String() != "unknown status" {
		t.Errorf("an unknown status reads as %q", s.String())
	}
}
