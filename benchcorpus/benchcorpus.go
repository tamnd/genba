// Package benchcorpus generates the fixed corpus the performance work is
// measured against.
//
// A benchmark is only worth the corpus it runs on. Ten documents in a map
// answer every query in microseconds and prove nothing, and a corpus of
// "lorem ipsum" repeated is uniform in a way real text never is, so it hides
// exactly the behaviour that makes search slow: a handful of terms appearing in
// most documents, a long tail appearing in one, documents that differ in length
// by two orders of magnitude, and a permission structure where the asker can
// read some of it rather than all or none.
//
// So the corpus here is generated from distributions rather than from a loop.
// Term frequencies are Zipf, body lengths are log normal, sources and group
// sizes are uneven, and the benchmark principal can read roughly sixty percent
// of it. Everything is derived from a seed, so the same seed produces the same
// database on any machine, which is what lets a number recorded last month be
// compared to one measured today.
//
// BASELINE.md in this directory is where the query path stood the last time
// somebody wrote it down, including the budgets it does not meet.
package benchcorpus

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/store"
)

// Spec is a corpus to generate.
//
// The defaults come from the measurement plan, which took them from surveys of
// real document corpora rather than from what felt about right. They are fields
// rather than constants so a benchmark can generate a tenth of the corpus while
// somebody is iterating, and the recorded baseline can state the size it was
// measured at.
type Spec struct {
	Seed      uint64
	Documents int

	// Vocabulary is how many distinct terms the generator draws from. Sixty
	// thousand is about what a hundred thousand real documents carry once
	// names, identifiers and misspellings are counted.
	Vocabulary int

	// Zipf is the exponent of the term distribution. Just above one is what
	// natural language produces, and it is the reason a search for a common word
	// touches a quarter of the corpus while the median word touches four
	// documents.
	Zipf float64

	// MedianBody and P99Body pin the log normal body length, in words.
	MedianBody int
	P99Body    int

	Containers int
	People     int
	Groups     int

	// Readable is the share of the corpus the benchmark principal can read. It
	// is not one, because a permission predicate that never excludes anything is
	// not being measured, and it is not a tenth, because then the ranking is
	// measured over a corpus nobody has.
	Readable float64
}

// Default is the corpus the budgets are stated against.
func Default(seed uint64, documents int) Spec {
	return Spec{
		Seed:       seed,
		Documents:  documents,
		Vocabulary: 60_000,
		Zipf:       1.07,
		MedianBody: 380,
		P99Body:    4_200,
		Containers: 2_400,
		People:     1_200,
		Groups:     180,
		Readable:   0.60,
	}
}

// Tenant is the tenant every generated document belongs to.
const Tenant = "acme"

// Epoch is the date the generated modification times are measured back from.
//
// It is a constant rather than time.Now, because a corpus whose dates move with
// the calendar makes the recency prior part of the ranking move too, and then a
// benchmark recorded in March cannot be compared with one recorded in June.
var Epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// share is one source and how much of the corpus comes from it.
type share struct {
	name   string
	weight int
}

// sources are uneven, because every real deployment is: one system holds most
// of the documents and the rest hold the interesting ones.
var sources = []share{
	{"gdrive", 30}, {"slack", 25}, {"github", 18},
	{"jira", 12}, {"confluence", 9}, {"notion", 6},
}

var kinds = []doc.Kind{
	doc.KindPage, doc.KindMessage, doc.KindTicket, doc.KindFile,
	doc.KindCode, doc.KindEmail, doc.KindCalendar, doc.KindPerson,
}

// Each calls fn with every document in the corpus, in order, and stops early if
// it returns false.
//
// It is deliberately serial. Generating in parallel would be faster and the
// resulting corpus would depend on scheduling, which is the one property this
// package exists to avoid.
func (s Spec) Each(fn func(doc.Document) bool) {
	g := s.generator()
	for i := range s.Documents {
		if !fn(g.document(i)) {
			return
		}
	}
}

// Generate writes the corpus into st in batches.
//
// Five hundred at a time, which is the batch size the ingestion budget is
// stated against, so building the fixture is also a rough measurement of the
// write path.
func (s Spec) Generate(ctx context.Context, st store.Store, progress func(done int)) error {
	const batch = 500

	var (
		docs = make([]doc.Document, 0, batch)
		done int
		err  error
	)
	flush := func() bool {
		if err = st.Put(ctx, docs...); err != nil {
			err = fmt.Errorf("benchcorpus: %w", err)
			return false
		}
		done += len(docs)
		docs = docs[:0]
		if progress != nil {
			progress(done)
		}
		return true
	}

	s.Each(func(d doc.Document) bool {
		docs = append(docs, d)
		return len(docs) < batch || flush()
	})
	if err != nil {
		return err
	}
	if len(docs) > 0 && !flush() {
		return err
	}
	return nil
}

// generator is the whole of one corpus's random state, in one place, so that
// the document builder is a method rather than a function taking nine
// arguments.
type generator struct {
	spec   Spec
	r      *rand.Rand
	terms  *rand.Zipf
	mu     float64
	sigma  float64
	people []doc.Person
	groups []float64
}

func (s Spec) generator() *generator {
	r := rand.New(rand.NewPCG(s.Seed, s.Seed^0x9e3779b97f4a7c15))
	return &generator{
		spec:  s,
		r:     r,
		terms: rand.NewZipf(r, s.Zipf, 1, uint64(s.Vocabulary-1)),
		mu:    math.Log(float64(s.MedianBody)),
		// The p99 of a log normal is exp(mu + 2.326 sigma), which is where the
		// sigma that reproduces the stated pair comes from.
		sigma:  math.Log(float64(s.P99Body)/float64(s.MedianBody)) / 2.326,
		people: s.people(),
		groups: s.groupSizes(),
	}
}

func (g *generator) document(i int) doc.Document {
	body := int(math.Exp(g.mu + g.sigma*g.r.NormFloat64()))
	body = min(max(body, 8), 20_000)

	author := g.people[g.r.IntN(len(g.people))]
	owner := g.people[g.r.IntN(len(g.people))]

	d := doc.Document{
		ID:        "b" + strconv.Itoa(i),
		Tenant:    Tenant,
		Source:    sources[weighted(g.r, sources)].name,
		Kind:      kinds[g.r.IntN(len(kinds))],
		Title:     g.text(3 + g.r.IntN(10)),
		Body:      g.text(body),
		Container: "c" + strconv.Itoa(g.r.IntN(g.spec.Containers)),
		Author:    author,
		Owner:     owner,
		// Skewed towards the recent, because a document store is: most of what
		// exists was touched in the last year and the rest is archive. An even
		// spread would make the recency prior look weaker than it is.
		ModifiedAt: Epoch.Add(-time.Duration(g.r.Float64()*g.r.Float64()*1095*24) * time.Hour),
	}
	d.Permissions = g.permissions(i, owner)
	return d
}

// permissions gives the document either a tenant wide mode or a single allow
// group drawn from the same skewed distribution the membership was computed
// against.
func (g *generator) permissions(i int, owner doc.Person) acl.Permissions {
	p := acl.Permissions{
		Source:  "gdrive",
		Version: 1,
		Owner:   acl.Ref{Source: "gdrive", Value: owner.Email},
	}
	// A quarter of the corpus is readable by everybody in the tenant, which is
	// what a wiki looks like, and it is also the cheap branch of the predicate,
	// so a corpus without it measures only the expensive one.
	if i%4 == 0 {
		p.Mode = acl.ModePublicToTenant
		return p
	}
	p.Mode = acl.ModeACL
	p.AllowGroups = []acl.Ref{{Source: "gdrive", Value: groupName(pick(g.r, g.groups))}}
	return p
}

// people are the authors and owners. A thousand or so, so that an author facet
// has a long tail rather than six values.
func (s Spec) people() []doc.Person {
	out := make([]doc.Person, s.People)
	for i := range out {
		name := word(i*7919%s.Vocabulary) + " " + word((i*104729+13)%s.Vocabulary)
		out[i] = doc.Person{
			Subject: "u" + strconv.Itoa(i),
			Name:    strings.ToUpper(name[:1]) + name[1:],
			Email:   "u" + strconv.Itoa(i) + "@acme.com",
		}
	}
	return out
}

// groupSizes is the share of documents each group guards, Zipf again: a few
// company wide groups hold most of the corpus and most groups hold a project.
func (s Spec) groupSizes() []float64 {
	out := make([]float64, s.Groups)
	var total float64
	for i := range out {
		out[i] = 1 / math.Pow(float64(i+1), 1.2)
		total += out[i]
	}
	for i := range out {
		out[i] /= total
	}
	return out
}

// membership picks the groups the benchmark principal belongs to.
//
// It walks the groups in a fixed shuffled order and takes them until the share
// of the corpus it can read reaches [Spec.Readable], counting the quarter that
// is public to the tenant. Taking the largest groups first would give the
// principal three groups and a very cheap predicate, which is not what a real
// membership looks like.
func (s Spec) membership(groups []float64) []bool {
	const public = 0.25
	order := make([]int, len(groups))
	for i := range order {
		order[i] = i
	}
	r := rand.New(rand.NewPCG(s.Seed^0xdeadbeef, s.Seed))
	r.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })

	member := make([]bool, len(groups))
	share := public
	for _, g := range order {
		if share >= s.Readable {
			break
		}
		member[g] = true
		share += (1 - public) * groups[g]
	}
	return member
}

// Principal is the reader every benchmark runs as.
func (s Spec) Principal() *acl.Principal {
	groups := s.groupSizes()
	member := s.membership(groups)
	p := &acl.Principal{
		Tenant:     Tenant,
		Subject:    "u_bench",
		Identities: []acl.Identity{{Source: "gdrive", Value: "bench@acme.com"}},
		Groups:     acl.GroupSet{Version: 1},
	}
	for g, in := range member {
		if in {
			p.Groups.Members = append(p.Groups.Members, "gdrive:"+groupName(g))
		}
	}
	return p
}

// Stranger is a reader in the same tenant who is in no group at all, which is
// the case that proves the predicate is doing work rather than being skipped.
func (s Spec) Stranger() *acl.Principal {
	return &acl.Principal{
		Tenant:     Tenant,
		Subject:    "u_stranger",
		Identities: []acl.Identity{{Source: "gdrive", Value: "stranger@acme.com"}},
		Groups:     acl.GroupSet{Version: 1},
	}
}

func groupName(i int) string { return "g" + strconv.Itoa(i) + "@acme.com" }

// text draws n words from the Zipf distribution.
func (g *generator) text(n int) string {
	var b strings.Builder
	b.Grow(n * 7)
	for i := range n {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(word(int(g.terms.Uint64())))
		// A sentence break every dozen words or so, which is what gives the
		// snippet code something to cut at.
		if i%13 == 12 {
			b.WriteByte('.')
		}
	}
	return b.String()
}

// syllables build the vocabulary. Synthetic, but with the shape of words: two
// or three syllables, a suffix, and a length distribution close enough that the
// analyzer, the full text index and the snippet code all do the work they would
// do on real text.
var syllables = []string{
	"ba", "ka", "ro", "mi", "ten", "dor", "fal", "que", "nix", "sar",
	"vel", "tri", "gon", "lum", "per", "cas", "mor", "ith", "ral", "sen",
	"tor", "vex", "wyn", "zal",
}

var suffixes = []string{"", "s", "ing", "ed", "er", "ion"}

// Word maps a term rank to its spelling, and does it the same way every time,
// so a query recorded against one corpus means the same thing in the next one.
// Rank zero is the most common term in the corpus.
func Word(rank int) string { return word(rank) }

func word(i int) string {
	n := len(syllables)
	var b strings.Builder
	b.WriteString(syllables[i%n])
	b.WriteString(syllables[(i/n)%n])
	b.WriteString(syllables[(i/(n*n))%n])
	b.WriteString(suffixes[(i/(n*n*n))%len(suffixes)])
	return b.String()
}

// weighted picks an index in proportion to the weights.
func weighted(r *rand.Rand, items []share) int {
	var total int
	for _, it := range items {
		total += it.weight
	}
	n := r.IntN(total)
	for i, it := range items {
		if n < it.weight {
			return i
		}
		n -= it.weight
	}
	return len(items) - 1
}

// pick chooses an index in proportion to a normalised distribution.
func pick(r *rand.Rand, weights []float64) int {
	n := r.Float64()
	for i, w := range weights {
		if n < w {
			return i
		}
		n -= w
	}
	return len(weights) - 1
}
