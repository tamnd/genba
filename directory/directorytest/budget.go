//go:build !race

package directorytest

import "time"

// budget is how long a subject in a thousand groups may take to resolve.
//
// It is a bound rather than a benchmark. An adapter that walks a level serially
// against a fake that answers instantly still passes, and one that is
// accidentally quadratic does not, which is the only thing this number is asked
// to tell apart. What the case really asserts is the lookup count, and that one
// is exact and says the same thing on every machine.
const budget = 10 * time.Second
