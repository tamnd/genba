package index_test

import (
	"slices"
	"testing"
	"time"

	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/index"
)

func TestParseReadsOperators(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  index.Query
	}{
		{
			name:  "plain text is left alone",
			input: "payments failover runbook",
			want:  index.Query{Text: "payments failover runbook"},
		},
		{
			name:  "app and source are the same operator",
			input: "outage app:slack source:gdrive",
			want:  index.Query{Text: "outage", Sources: []string{"slack", "gdrive"}},
		},
		{
			name:  "type and kind are the same operator",
			input: "type:ticket kind:page",
			want:  index.Query{Kinds: []doc.Kind{doc.KindTicket, doc.KindPage}},
		},
		{
			name:  "in names a container",
			input: "in:incidents deploy",
			want:  index.Query{Text: "deploy", Containers: []string{"incidents"}},
		},
		{
			name:  "from by and author all name a person",
			input: "from:mei by:sam author:kai",
			want:  index.Query{Authors: []string{"mei", "sam", "kai"}},
		},
		{
			name:  "owner is its own field",
			input: "owner:mei@acme.com",
			want:  index.Query{Owners: []string{"mei@acme.com"}},
		},
		{
			name:  "sort switches the ordering",
			input: "payments sort:recent",
			want:  index.Query{Text: "payments", Sort: index.ByRecent},
		},
		{
			name:  "an unknown operator is text",
			input: "note: this broke",
			want:  index.Query{Text: "note: this broke"},
		},
		{
			name:  "a colon with nothing after it is text",
			input: "app: alone",
			want:  index.Query{Text: "app: alone"},
		},
		{
			name:  "a sort nobody knows is text",
			input: "sort:sideways",
			want:  index.Query{Text: "sort:sideways"},
		},
		{
			name:  "the field name is case insensitive",
			input: "APP:slack",
			want:  index.Query{Sources: []string{"slack"}},
		},
		{
			name:  "the source value is folded",
			input: "app:Slack",
			want:  index.Query{Sources: []string{"slack"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := index.Parse(tc.input)
			if got.Text != tc.want.Text {
				t.Errorf("Text = %q, want %q", got.Text, tc.want.Text)
			}
			if !slices.Equal(got.Sources, tc.want.Sources) {
				t.Errorf("Sources = %v, want %v", got.Sources, tc.want.Sources)
			}
			if !slices.Equal(got.Kinds, tc.want.Kinds) {
				t.Errorf("Kinds = %v, want %v", got.Kinds, tc.want.Kinds)
			}
			if !slices.Equal(got.Containers, tc.want.Containers) {
				t.Errorf("Containers = %v, want %v", got.Containers, tc.want.Containers)
			}
			if !slices.Equal(got.Authors, tc.want.Authors) {
				t.Errorf("Authors = %v, want %v", got.Authors, tc.want.Authors)
			}
			if !slices.Equal(got.Owners, tc.want.Owners) {
				t.Errorf("Owners = %v, want %v", got.Owners, tc.want.Owners)
			}
			if got.Sort != tc.want.Sort {
				t.Errorf("Sort = %q, want %q", got.Sort, tc.want.Sort)
			}
		})
	}
}

// A quoted run is the escape hatch. Somebody searching for the literal text
// "app: down" has to be able to, and the box must not turn it into a filter.
func TestParseNeverReadsAnOperatorInsideQuotes(t *testing.T) {
	q := index.Parse(`"app:slack" outage`)
	if len(q.Sources) != 0 {
		t.Fatalf("a quoted operator became a filter: %v", q.Sources)
	}
	if q.Text != "app:slack outage" {
		t.Fatalf("Text = %q, want the quoted run kept as text", q.Text)
	}
}

func TestParseReadsExactDates(t *testing.T) {
	day := func(s string) time.Time {
		t.Helper()
		d, err := time.Parse(time.DateOnly, s)
		if err != nil {
			t.Fatalf("bad test date %q: %v", s, err)
		}
		return d
	}

	cases := []struct {
		input        string
		since, until time.Time
	}{
		{"updated:2026-08-19", day("2026-08-19"), day("2026-08-20").Add(-time.Nanosecond)},
		{"updated:2026-01-01..2026-03-31", day("2026-01-01"), day("2026-04-01").Add(-time.Nanosecond)},
		{"updated:>2026-01-01", day("2026-01-01"), time.Time{}},
		{"updated:<2026-01-01", time.Time{}, day("2026-01-02").Add(-time.Nanosecond)},
		{"modified:2026-08-19", day("2026-08-19"), day("2026-08-20").Add(-time.Nanosecond)},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			q := index.Parse(tc.input)
			if !q.Since.Equal(tc.since) {
				t.Errorf("Since = %v, want %v", q.Since, tc.since)
			}
			if !q.Until.Equal(tc.until) {
				t.Errorf("Until = %v, want %v", q.Until, tc.until)
			}
			if q.Text != "" {
				t.Errorf("Text = %q, want the operator consumed", q.Text)
			}
		})
	}
}

// The relative windows are read against the wall clock, so this asserts the
// window they open rather than an exact instant.
func TestParseReadsRelativeWindows(t *testing.T) {
	cases := []struct {
		input string
		ago   time.Duration
	}{
		{"updated:today", 24 * time.Hour},
		{"updated:week", 7 * 24 * time.Hour},
		{"updated:7d", 7 * 24 * time.Hour},
		{"updated:2w", 14 * 24 * time.Hour},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			before := time.Now()
			q := index.Parse(tc.input)
			if q.Since.IsZero() {
				t.Fatalf("%q set no lower bound", tc.input)
			}
			if !q.Until.IsZero() {
				t.Fatalf("%q set an upper bound of %v, want none", tc.input, q.Until)
			}
			// A window opened now, allowing for the clock moving during the call
			// and for the calendar arithmetic in months and years.
			want := before.Add(-tc.ago)
			if diff := q.Since.Sub(want); diff < -time.Minute || diff > time.Minute {
				t.Fatalf("Since = %v, want roughly %v", q.Since, want)
			}
		})
	}
}

func TestParseRejectsUnreadableDates(t *testing.T) {
	for _, input := range []string{"updated:soonish", "updated:2026-13-45", "updated:>notaday", "updated:2026-01-01..nope"} {
		q := index.Parse(input)
		if !q.Since.IsZero() || !q.Until.IsZero() {
			t.Errorf("%q was read as a date window", input)
		}
		if q.Text != input {
			t.Errorf("%q became %q, want it kept as text", input, q.Text)
		}
	}
}
