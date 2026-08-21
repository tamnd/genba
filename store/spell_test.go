package store_test

import (
	"slices"
	"testing"

	"github.com/tamnd/genba/store"
)

func TestEditsMeasuresWhatHandsDo(t *testing.T) {
	cases := []struct {
		a, b string
		max  int
		want int
	}{
		{"cache", "cache", 2, 0},
		{"cahce", "cache", 2, 1}, // the transposition, which is one edit and not two
		{"teh", "the", 2, 1},     // and the one everybody types
		{"recieve", "receive", 2, 1},
		{"cache", "caches", 2, 1}, // an insertion
		{"caches", "cache", 2, 1}, // and a deletion, which is the same edit read backwards
		{"cache", "cachet", 2, 1},
		// Two words that look alike to a person and are three edits apart, so
		// neither is offered as the other.
		{"kubectl", "kubelet", 2, 3},
		{"cat", "dog", 2, 3},            // nothing like it, reported as over the bound
		{"cache", "invalidation", 2, 3}, // and a length difference is answered without work
		{"日本語", "日本", 2, 1},             // runes rather than bytes, or this would be three
		{"ünicode", "unicode", 2, 1},
	}
	for _, c := range cases {
		if got := store.Edits(c.a, c.b, c.max); got != c.want {
			t.Errorf("Edits(%q, %q, %d) = %d, want %d", c.a, c.b, c.max, got, c.want)
		}
		// Distance is symmetric, and a version that is not has an off by one in
		// the row it reuses.
		if got := store.Edits(c.b, c.a, c.max); got != c.want {
			t.Errorf("Edits(%q, %q, %d) = %d, want %d", c.b, c.a, c.max, got, c.want)
		}
	}
}

func TestEditsStopsAtTheBound(t *testing.T) {
	// Anything further away is max+1 rather than the real distance, which is
	// what lets the measurement stop early. Asking for the real number is not
	// something a correction ever needs.
	if got := store.Edits("cache", "dog", 1); got != 2 {
		t.Errorf("Edits(cache, dog, 1) = %d, want 2", got)
	}
	if got := store.Edits("cache", "dog", 4); got != 5 {
		t.Errorf("Edits(cache, dog, 4) = %d, want 5", got)
	}
}

func TestMaxEditsIsStricterOnShortWords(t *testing.T) {
	for _, term := range []string{"cat", "cost", "四文字です"[:6]} {
		if got := store.MaxEdits(term); got > 2 {
			t.Errorf("MaxEdits(%q) = %d", term, got)
		}
	}
	if got := store.MaxEdits("cost"); got != 1 {
		t.Errorf("MaxEdits(cost) = %d, want 1: two edits on a four letter word is a different word", got)
	}
	if got := store.MaxEdits("caches"); got != 2 {
		t.Errorf("MaxEdits(caches) = %d, want 2", got)
	}
}

func TestNearestPrefersTheNearerThenTheCommoner(t *testing.T) {
	got := store.Nearest("cahce", map[string]int{
		"cache":  3,   // one edit
		"caches": 900, // two, and much commoner, and still second
		"cage":   40,  // two
		"cahce":  5,   // the word itself, which is never an answer
		"dog":    900, // nothing like it
		"cached": 0,   // carried by nothing, so it is not a place to send anybody
	}, 3)
	want := []string{"cache", "caches", "cage"}
	if !slices.Equal(got, want) {
		t.Errorf("Nearest = %v, want %v", got, want)
	}
}

func TestNearestBreaksTiesOnTheWord(t *testing.T) {
	// Same distance and same document count. Something has to decide, and it
	// has to decide the same way twice or the correction offered for one query
	// changes between two runs of it.
	got := store.Nearest("cachs", map[string]int{"cache": 7, "cacho": 7, "cachy": 7}, 2)
	if want := []string{"cache", "cacho"}; !slices.Equal(got, want) {
		t.Errorf("Nearest = %v, want %v", got, want)
	}
}

func TestNearestAnswersNothingWhenThereIsNothingToAsk(t *testing.T) {
	if got := store.Nearest("", map[string]int{"cache": 3}, 3); got != nil {
		t.Errorf("Nearest with no word = %v", got)
	}
	if got := store.Nearest("cahce", map[string]int{"cache": 3}, 0); got != nil {
		t.Errorf("Nearest with no limit = %v", got)
	}
	if got := store.Nearest("cahce", nil, 3); len(got) != 0 {
		t.Errorf("Nearest with no candidates = %v", got)
	}
}
