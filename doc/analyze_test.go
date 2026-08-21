package doc

import (
	"slices"
	"strings"
	"testing"
)

// The analyzer is asked the same question three ways, and the three have to
// agree, because a driver that stores what Tokenize produced and a snippet that
// marks what Find located are the same match seen from two ends.

func TestSpansAgreeWithTokenize(t *testing.T) {
	for _, text := range []string{
		"",
		"Hello, World!",
		"eu-west-1 failover",
		"  spaces   between   words  ",
		"Café RÉSUMÉ naïve",
		"現場の記録 and prose",
		"trailing",
	} {
		var terms []string
		for s := range Spans(text) {
			terms = append(terms, s.Term)
			if got := text[s.Start:s.End]; got == "" {
				t.Errorf("Spans(%q) gave an empty span at %d", text, s.Start)
			}
		}
		if want := Tokenize(text); !slices.Equal(terms, want) {
			t.Errorf("Spans(%q) = %v, want %v", text, terms, want)
		}
	}
}

func TestSpansOffsetsPointAtTheOriginalCharacters(t *testing.T) {
	text := "Failover the RÉSUMÉ queue"
	want := []string{"Failover", "the", "RÉSUMÉ", "queue"}
	var got []string
	for s := range Spans(text) {
		got = append(got, text[s.Start:s.End])
	}
	if !slices.Equal(got, want) {
		t.Errorf("the spans cover %v, want %v", got, want)
	}
}

func TestFindIsTheFirstWholeTermAndNothingInsideAWord(t *testing.T) {
	tests := []struct {
		text string
		want map[string]bool
		at   int
	}{
		{"the runbook says run the job", map[string]bool{"run": true}, 17},
		{"payments settle overnight", map[string]bool{"settle": true}, 9},
		{"payments settle overnight", map[string]bool{"absent": true}, -1},
		{"payments settle overnight", map[string]bool{"payments": true, "settle": true}, 0},
		{"PAYMENTS settle", map[string]bool{"payments": true}, 0},
		{"", map[string]bool{"payments": true}, -1},
		{"現場の記録", map[string]bool{"記": true}, 9},
	}
	for _, tt := range tests {
		if got := Find(tt.text, tt.want); got != tt.at {
			t.Errorf("Find(%q, %v) = %d, want %d", tt.text, tt.want, got, tt.at)
		}
	}
}

// Find has to give the same answer as walking every span, since the whole point
// of it is that it is the cheap way to ask the analyzer the same question.
func TestFindAgreesWithSpans(t *testing.T) {
	want := map[string]bool{"queue": true, "記": true}
	for _, text := range []string{
		"the queue drains",
		"nothing here at all",
		"a queueing theory queue",
		"現場の記録",
		"Café queue",
	} {
		at := -1
		for s := range Spans(text) {
			if want[s.Term] {
				at = s.Start
				break
			}
		}
		if got := Find(text, want); got != at {
			t.Errorf("Find(%q) = %d, want the first matching span at %d", text, got, at)
		}
	}
}

func TestAnalyzeCountsTitleAndBodySeparately(t *testing.T) {
	d := Document{Title: "Payments runbook", Body: "The payments queue drains. Payments settle."}
	a := d.Analyze()

	if a.TitleTokens != 2 {
		t.Errorf("counted %d title tokens, want 2", a.TitleTokens)
	}
	if a.BodyTokens != 6 {
		t.Errorf("counted %d body tokens, want 6", a.BodyTokens)
	}
	if got, want := a.Terms["payments"], (TermCount{Title: 1, Body: 2}); got != want {
		t.Errorf("payments counted %+v, want %+v", got, want)
	}
}

func BenchmarkFind(b *testing.B) {
	text := strings.Repeat("The ledger closes overnight and the queue drains. ", 400)
	want := map[string]bool{"absent": true}
	b.ReportAllocs()
	for b.Loop() {
		if Find(text, want) != -1 {
			b.Fatal("the term is not in the text")
		}
	}
}
