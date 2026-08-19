package column_test

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/genba/store/column"
)

// The four boxes on the issue, in order: every type round trips including
// nulls, a dictionary column is an order of magnitude smaller than the raw
// strings, scans produce bitmaps the permission filter intersects directly, and
// the throughput per type is measured in bench_test.go.

func TestAStringColumnRoundTrips(t *testing.T) {
	want := []string{"github", "slack", "drive", "github", "", "drive", "zzz", "a"}
	b := column.NewBuilder(column.TypeString)
	for _, s := range want {
		b.AppendString(s)
	}
	c := open(t, b)

	if got := c.Rows(); got != len(want) {
		t.Fatalf("the column has %d rows and %d went in", got, len(want))
	}
	for i, w := range want {
		got, ok := c.StringAt(i)
		if !ok || got != w {
			t.Errorf("row %d is %q %v and should be %q", i, got, ok, w)
		}
	}
	if got := c.Dict(); !slices.Equal(got, []string{"", "a", "drive", "github", "slack", "zzz"}) {
		t.Errorf("the dictionary is %q, which is either not distinct or not sorted", got)
	}
}

func TestAnIntColumnRoundTrips(t *testing.T) {
	want := []int64{0, -1, 1, math.MinInt64, math.MaxInt64, 42, -42}
	b := column.NewBuilder(column.TypeInt)
	for _, v := range want {
		b.AppendInt(v)
	}
	c := open(t, b)
	for i, w := range want {
		got, ok := c.IntAt(i)
		if !ok || got != w {
			t.Errorf("row %d is %d %v and should be %d", i, got, ok, w)
		}
	}
}

func TestATimeColumnRoundTripsToTheMillisecond(t *testing.T) {
	base := time.Date(2026, 8, 20, 9, 30, 0, 0, time.UTC)
	want := []time.Time{base, base.Add(time.Hour), base.AddDate(-3, 0, 0), base.Add(1500 * time.Microsecond), {}}
	b := column.NewBuilder(column.TypeTime)
	for _, v := range want {
		b.AppendTime(v)
	}
	c := open(t, b)
	for i, w := range want {
		got, ok := c.TimeAt(i)
		if !ok || !got.Equal(w.Truncate(time.Millisecond)) {
			t.Errorf("row %d is %s %v and should be %s to the millisecond", i, got, ok, w)
		}
	}
}

func TestABoolColumnRoundTrips(t *testing.T) {
	want := []bool{true, false, false, true, true}
	b := column.NewBuilder(column.TypeBool)
	for _, v := range want {
		b.AppendBool(v)
	}
	c := open(t, b)
	for i, w := range want {
		got, ok := c.BoolAt(i)
		if !ok || got != w {
			t.Errorf("row %d is %v %v and should be %v", i, got, ok, w)
		}
	}
	// One bit a row, and five rows is well inside the padding, so this is
	// really a check that a boolean column does not reserve a byte per row.
	if c.Bits() != 1 {
		t.Errorf("a boolean column packs at %d bits a row", c.Bits())
	}
}

func TestAnEmptyColumnIsAColumn(t *testing.T) {
	for _, typ := range []column.Type{column.TypeString, column.TypeInt, column.TypeTime, column.TypeBool} {
		c := open(t, column.NewBuilder(typ))
		if c.Rows() != 0 || c.Type() != typ {
			t.Errorf("an empty %s column came back as %d rows of %s", typ, c.Rows(), c.Type())
		}
		if !c.Nulls().Empty() || !c.Present().Empty() {
			t.Errorf("an empty %s column has rows in it", typ)
		}
	}
}

// TestNullsRoundTripAndNeverMatch is the half of the first box that is easy to
// get wrong. A null carries a placeholder code, and a range predicate that
// happens to contain the placeholder must still not match the row.
func TestNullsRoundTripAndNeverMatch(t *testing.T) {
	b := column.NewBuilder(column.TypeInt)
	b.AppendInt(5)
	b.AppendNull()
	b.AppendInt(7)
	b.AppendNull()
	c := open(t, b)

	if _, ok := c.IntAt(1); ok {
		t.Error("a null row came back with a value")
	}
	if got, ok := c.IntAt(2); !ok || got != 7 {
		t.Errorf("the row after a null is %d %v and should be 7", got, ok)
	}
	if got := c.Nulls().Rows(); !slices.Equal(got, []int{1, 3}) {
		t.Errorf("the null rows are %v and should be 1 and 3", got)
	}
	if got := c.Present().Rows(); !slices.Equal(got, []int{0, 2}) {
		t.Errorf("the rows with a value are %v and should be 0 and 2", got)
	}
	// The base is five, so a null's placeholder code of zero stands for five.
	// A range that contains five must not pick up the nulls.
	rows := match(t, func() (*column.Bitmap, error) { return c.MatchInts(math.MinInt64, math.MaxInt64) })
	if got := rows.Rows(); !slices.Equal(got, []int{0, 2}) {
		t.Errorf("a range over everything matched %v, which includes a row with no value", got)
	}
}

func TestANullBeforeAnyValueIsStillANull(t *testing.T) {
	b := column.NewBuilder(column.TypeString)
	b.AppendNull()
	b.AppendString("drive")
	c := open(t, b)
	if got := c.Nulls().Rows(); !slices.Equal(got, []int{0}) {
		t.Errorf("the null rows are %v and should be just 0", got)
	}
	if got, ok := c.StringAt(1); !ok || got != "drive" {
		t.Errorf("row 1 is %q %v", got, ok)
	}
}

func TestAColumnOfNothingButNulls(t *testing.T) {
	b := column.NewBuilder(column.TypeTime)
	for range 100 {
		b.AppendNull()
	}
	c := open(t, b)
	if got := c.Nulls().Count(); got != 100 {
		t.Errorf("%d of the hundred rows are null", got)
	}
	rows := match(t, func() (*column.Bitmap, error) {
		return c.MatchTimes(time.Unix(0, 0), time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC))
	})
	if !rows.Empty() {
		t.Errorf("a range matched %d rows of a column that has no values", rows.Count())
	}
}

// TestADictionaryColumnIsAnOrderOfMagnitudeSmaller is the issue's second box: a
// thousand distinct values over a million rows against the raw strings.
func TestADictionaryColumnIsAnOrderOfMagnitudeSmaller(t *testing.T) {
	const rows, distinct = 1_000_000, 1000

	values := make([]string, distinct)
	for i := range values {
		values[i] = fmt.Sprintf("engineering/platform/service-%04d", i)
	}
	// Shuffled on purpose. Sorted input would be run length encoded and the
	// number would be a hundred times better and would prove nothing about the
	// dictionary, which is what this box is about.
	r := rand.New(rand.NewPCG(2121, 7))
	b := column.NewBuilder(column.TypeString)
	raw := 0
	for range rows {
		v := values[r.IntN(distinct)]
		b.AppendString(v)
		raw += len(v)
	}
	c := open(t, b)

	if c.Encoding() != column.EncodingPlain {
		t.Fatalf("a shuffled column was encoded as %s, so this is not measuring the dictionary", c.Encoding())
	}
	ratio := float64(raw) / float64(c.Size())
	t.Logf("%d bytes of strings became %d bytes at %d bits a row, which is %.1fx", raw, c.Size(), c.Bits(), ratio)
	if ratio < 10 {
		t.Errorf("the encoded column is %.1fx smaller than the raw strings and the bar is 10x", ratio)
	}
	// A thousand distinct values needs ten bits and nothing says it needs
	// eleven. This is the check that the width came from the cardinality.
	if c.Bits() != 10 {
		t.Errorf("a thousand distinct values packed at %d bits a row", c.Bits())
	}
}

// TestASortedColumnIsRunLengthEncoded is the other half of the encoding
// decision. Nothing asks for it, the builder measures both and keeps the
// smaller.
func TestASortedColumnIsRunLengthEncoded(t *testing.T) {
	b := column.NewBuilder(column.TypeString)
	for _, s := range []string{"drive", "github", "slack"} {
		for range 10_000 {
			b.AppendString(s)
		}
	}
	c := open(t, b)
	if c.Encoding() != column.EncodingRuns {
		t.Fatalf("thirty thousand rows in three runs were encoded as %s", c.Encoding())
	}
	if c.Size() > 200 {
		t.Errorf("three runs and three dictionary entries came to %d bytes", c.Size())
	}
	for i, want := range map[int]string{0: "drive", 9_999: "drive", 10_000: "github", 29_999: "slack"} {
		if got, ok := c.StringAt(i); !ok || got != want {
			t.Errorf("row %d of the run encoded column is %q %v and should be %q", i, got, ok, want)
		}
	}
}

func TestAColumnOfOneValueStoresNoCodesAtAll(t *testing.T) {
	b := column.NewBuilder(column.TypeInt)
	for range 100_000 {
		b.AppendInt(1755680000)
	}
	c := open(t, b)
	if c.Bits() != 0 {
		t.Errorf("a column with one distinct value packs at %d bits a row", c.Bits())
	}
	if c.Size() > 64 {
		t.Errorf("a hundred thousand copies of one number came to %d bytes", c.Size())
	}
	rows := match(t, func() (*column.Bitmap, error) { return c.MatchInts(1755680000, 1755680000) })
	if got := rows.Count(); got != 100_000 {
		t.Errorf("%d of a hundred thousand identical rows matched their own value", got)
	}
	rows = match(t, func() (*column.Bitmap, error) { return c.MatchInts(0, 5) })
	if !rows.Empty() {
		t.Errorf("%d rows matched a range that does not contain their value", rows.Count())
	}
}

// TestAScanAgreesWithWalkingTheRows is the one that would catch a bit packing
// mistake, an off by one in a run, or a range that is exclusive at one end. It
// builds the same data under both encodings and compares every predicate
// against the answer you get by looking at each value in turn.
func TestAScanAgreesWithWalkingTheRows(t *testing.T) {
	const rows = 5000
	r := rand.New(rand.NewPCG(2121, 3))
	terms := []string{"", "a", "drive", "github", "github-enterprise", "slack", "zz"}

	for _, sorted := range []bool{false, true} {
		name := "shuffled"
		if sorted {
			name = "sorted"
		}
		t.Run(name, func(t *testing.T) {
			values := make([]string, rows)
			nulls := make([]bool, rows)
			for i := range values {
				values[i] = terms[r.IntN(len(terms))]
				nulls[i] = r.IntN(10) == 0
			}
			if sorted {
				slices.Sort(values)
			}

			b := column.NewBuilder(column.TypeString)
			for i, v := range values {
				if nulls[i] {
					b.AppendNull()
					continue
				}
				b.AppendString(v)
			}
			c := open(t, b)

			check := func(what string, got *column.Bitmap, want func(string) bool) {
				t.Helper()
				expect := column.NewBitmap(rows)
				for i, v := range values {
					if !nulls[i] && want(v) {
						expect.Set(i)
					}
				}
				if !got.Equal(expect) {
					t.Errorf("%s matched %d rows and walking them gives %d", what, got.Count(), expect.Count())
				}
			}

			for _, v := range append(slices.Clone(terms), "nothing", "githu", "githubb") {
				check("equals "+v, match(t, func() (*column.Bitmap, error) { return c.MatchStrings(v) }),
					func(s string) bool { return s == v })
				check("prefix "+v, match(t, func() (*column.Bitmap, error) { return c.MatchPrefix(v) }),
					func(s string) bool { return strings.HasPrefix(s, v) })
			}
			set := []string{"drive", "slack", "nothing"}
			check("one of", match(t, func() (*column.Bitmap, error) { return c.MatchStrings(set...) }),
				func(s string) bool { return slices.Contains(set, s) })
			check("no values at all", match(t, func() (*column.Bitmap, error) { return c.MatchStrings() }),
				func(string) bool { return false })
			check("every value", match(t, func() (*column.Bitmap, error) { return c.MatchPrefix("") }),
				func(string) bool { return true })
		})
	}
}

func TestAnIntScanAgreesWithWalkingTheRows(t *testing.T) {
	const rows = 5000
	r := rand.New(rand.NewPCG(2121, 11))
	values := make([]int64, rows)
	for i := range values {
		values[i] = int64(r.IntN(300)) - 100
	}

	b := column.NewBuilder(column.TypeInt)
	for _, v := range values {
		b.AppendInt(v)
	}
	c := open(t, b)

	for _, span := range [][2]int64{
		{-100, 199}, {0, 0}, {-1000, 1000}, {50, 40}, {-500, -200}, {200, 500}, {-100, -100}, {199, 199}, {math.MinInt64, math.MaxInt64},
	} {
		expect := column.NewBitmap(rows)
		for i, v := range values {
			if v >= span[0] && v <= span[1] {
				expect.Set(i)
			}
		}
		got := match(t, func() (*column.Bitmap, error) { return c.MatchInts(span[0], span[1]) })
		if !got.Equal(expect) {
			t.Errorf("[%d, %d] matched %d rows and walking them gives %d", span[0], span[1], got.Count(), expect.Count())
		}
	}
}

// TestAPermissionFilterIsAnIntersection is the issue's third box. The scan
// hands back a bitmap and the ACL hands back a bitmap, and the answer is what
// survives both without either side knowing about the other.
func TestAPermissionFilterIsAnIntersection(t *testing.T) {
	b := column.NewBuilder(column.TypeString)
	for i := range 1000 {
		b.AppendString([]string{"drive", "github", "slack"}[i%3])
	}
	c := open(t, b)

	rows := match(t, func() (*column.Bitmap, error) { return c.MatchStrings("github") })
	if got := rows.Count(); got != 333 {
		t.Fatalf("the filter matched %d rows and there are 333 of them", got)
	}

	// Everything a principal may read. In the real thing this comes out of the
	// ACL section of the segment.
	allowed := column.NewBitmap(1000)
	allowed.SetRange(0, 100)

	rows.And(allowed)
	for _, row := range rows.Rows() {
		if row >= 100 {
			t.Fatalf("row %d survived the intersection and is not allowed", row)
		}
		if v, _ := c.StringAt(row); v != "github" {
			t.Fatalf("row %d survived the intersection and is %q", row, v)
		}
	}
	if got := rows.Count(); got != 33 {
		t.Errorf("%d rows are both github and allowed, and 33 of the first hundred are github", got)
	}
}

func TestTheSameValuesProduceTheSameBytes(t *testing.T) {
	build := func() []byte {
		b := column.NewBuilder(column.TypeString)
		// Appended out of order on purpose. The dictionary is sorted before it
		// is written, so the order the values first arrived in must not survive
		// into the bytes.
		for _, s := range []string{"slack", "drive", "github", "drive", "slack"} {
			b.AppendString(s)
		}
		out, err := b.Build()
		if err != nil {
			t.Fatalf("building: %v", err)
		}
		return out
	}
	if a, c := build(), build(); !bytes.Equal(a, c) {
		t.Error("two builds of the same values produced different bytes")
	}
}

func TestAppendingTheWrongTypeIsRefused(t *testing.T) {
	b := column.NewBuilder(column.TypeString)
	b.AppendString("drive")
	b.AppendInt(3)
	if _, err := b.Build(); !errors.Is(err, column.ErrType) {
		t.Errorf("building a column with an integer appended to it gave %v", err)
	}
}

func TestAnUnknownTypeIsRefusedAtTheBuilder(t *testing.T) {
	if _, err := column.NewBuilder(column.Type(77)).Build(); !errors.Is(err, column.ErrType) {
		t.Errorf("a builder for type 77 gave %v", err)
	}
}

func TestOpenRefusesWhatItCannotRead(t *testing.T) {
	good := populated(t)

	cases := []struct {
		name string
		want error
		edit func([]byte)
	}{
		{"a version from the future", column.ErrVersion, func(b []byte) { b[0] = 2 }},
		{"a version from the past", column.ErrVersion, func(b []byte) { b[0] = 0 }},
		{"a type nobody defined", column.ErrFormat, func(b []byte) { b[1] = 77 }},
		{"an encoding nobody defined", column.ErrFormat, func(b []byte) { b[2] = 9 }},
		{"a width that cannot be read back", column.ErrFormat, func(b []byte) { b[3] = 60 }},
		{"a flag this version does not define", column.ErrFormat, func(b []byte) { b[12] = 0x80 }},
		{"a reserved byte that is not zero", column.ErrFormat, func(b []byte) { b[13] = 1 }},
		{"more rows than there are codes", column.ErrFormat, func(b []byte) { b[4] = 0xff }},
		{"a dictionary larger than the bytes", column.ErrFormat, func(b []byte) { b[8] = 0xff }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := bytes.Clone(good)
			tc.edit(b)
			c, err := column.Open(b)
			if !errors.Is(err, tc.want) {
				t.Fatalf("opening gave %v (column %v) and should have given %v", err, c, tc.want)
			}
		})
	}
}

func TestTrailingBytesAreRefused(t *testing.T) {
	b := append(populated(t), 0)
	if _, err := column.Open(b); !errors.Is(err, column.ErrFormat) {
		t.Errorf("a column with a byte stuck on the end opened with %v", err)
	}
}

// TestTruncationIsAnErrorAndNeverAPanic cuts the bytes at every length. None of
// them is a column and none of them may take the process down, because these
// bytes come off a disk that may have been anything at all.
func TestTruncationIsAnErrorAndNeverAPanic(t *testing.T) {
	good := populated(t)
	for n := range len(good) {
		if _, err := column.Open(good[:n]); err == nil {
			t.Fatalf("the first %d bytes of a %d byte column opened cleanly", n, len(good))
		}
	}
}

// TestEveryRowIsReachableUnderBothEncodings covers the random access path,
// which for the run encoding is a search rather than a shift and is the one
// place the two encodings do genuinely different work.
func TestEveryRowIsReachableUnderBothEncodings(t *testing.T) {
	r := rand.New(rand.NewPCG(2121, 5))
	for _, sorted := range []bool{false, true} {
		values := make([]int64, 2000)
		for i := range values {
			values[i] = int64(r.IntN(50))
		}
		if sorted {
			slices.Sort(values)
		}
		b := column.NewBuilder(column.TypeInt)
		for _, v := range values {
			b.AppendInt(v)
		}
		c := open(t, b)
		want := column.EncodingPlain
		if sorted {
			want = column.EncodingRuns
		}
		if c.Encoding() != want {
			t.Fatalf("sorted=%v was encoded as %s and should have been %s", sorted, c.Encoding(), want)
		}
		for i, v := range values {
			if got, ok := c.IntAt(i); !ok || got != v {
				t.Fatalf("%s row %d is %d %v and should be %d", c.Encoding(), i, got, ok, v)
			}
		}
		if _, ok := c.IntAt(-1); ok {
			t.Error("row -1 has a value")
		}
		if _, ok := c.IntAt(len(values)); ok {
			t.Error("the row after the last one has a value")
		}
	}
}

func TestAPredicateAgainstTheWrongTypeIsRefused(t *testing.T) {
	str := open(t, appendAll(column.NewBuilder(column.TypeString), func(b *column.Builder) { b.AppendString("drive") }))
	num := open(t, appendAll(column.NewBuilder(column.TypeInt), func(b *column.Builder) { b.AppendInt(1) }))

	for _, call := range []struct {
		what string
		run  func() (*column.Bitmap, error)
	}{
		{"strings against an int column", func() (*column.Bitmap, error) { return num.MatchStrings("drive") }},
		{"a prefix against an int column", func() (*column.Bitmap, error) { return num.MatchPrefix("d") }},
		{"an int range against a string column", func() (*column.Bitmap, error) { return str.MatchInts(0, 1) }},
		{"a time range against a string column", func() (*column.Bitmap, error) { return str.MatchTimes(time.Time{}, time.Now()) }},
		{"a boolean against a string column", func() (*column.Bitmap, error) { return str.MatchBool(true) }},
	} {
		if _, err := call.run(); !errors.Is(err, column.ErrType) {
			t.Errorf("%s gave %v", call.what, err)
		}
	}
}

// FuzzOpenSurvivesAnything is the same contract the segment format has. These
// bytes come off a disk, and a reader that panics on a damaged file turns a
// recoverable segment into an outage.
func FuzzOpenSurvivesAnything(f *testing.F) {
	if b, err := buildFuzzSeed(); err == nil {
		f.Add(b)
	}
	f.Add([]byte{})
	f.Add(make([]byte, 24))

	f.Fuzz(func(t *testing.T, b []byte) {
		c, err := column.Open(b)
		if err != nil {
			return
		}
		// It opened, so every accessor on it has to be safe for every row and
		// for a few that are not rows.
		for i := -2; i < c.Rows()+2; i++ {
			c.StringAt(i)
			c.IntAt(i)
			c.TimeAt(i)
			c.BoolAt(i)
			c.CodeAt(i)
		}
		c.Dict()
		c.Nulls()
		c.Present()
		_, _ = c.MatchStrings("", "a")
		_, _ = c.MatchPrefix("a")
		_, _ = c.MatchInts(math.MinInt64, math.MaxInt64)
		_, _ = c.MatchTimes(time.Time{}, time.Now())
		_, _ = c.MatchBool(true)
	})
}

func buildFuzzSeed() ([]byte, error) {
	b := column.NewBuilder(column.TypeString)
	b.AppendString("drive")
	b.AppendNull()
	b.AppendString("github")
	return b.Build()
}

// populated is a column with a dictionary, nulls and more than one row, so that
// every section of the format is present in the bytes a test then damages.
func populated(t testing.TB) []byte {
	t.Helper()
	b := column.NewBuilder(column.TypeString)
	r := rand.New(rand.NewPCG(2121, 13))
	for i := range 500 {
		if i%17 == 0 {
			b.AppendNull()
			continue
		}
		b.AppendString(fmt.Sprintf("source-%02d", r.IntN(40)))
	}
	out, err := b.Build()
	if err != nil {
		t.Fatalf("building: %v", err)
	}
	return out
}

func open(t testing.TB, b *column.Builder) *column.Column {
	t.Helper()
	encoded, err := b.Build()
	if err != nil {
		t.Fatalf("building: %v", err)
	}
	c, err := column.Open(encoded)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	return c
}

func appendAll(b *column.Builder, f func(*column.Builder)) *column.Builder {
	f(b)
	return b
}

func match(t testing.TB, f func() (*column.Bitmap, error)) *column.Bitmap {
	t.Helper()
	got, err := f()
	if err != nil {
		t.Fatalf("scanning: %v", err)
	}
	return got
}
