//go:build race

package directorytest

import "time"

// budget under the race detector, which is the same bound with the cost of
// running it taken into account.
//
// An adapter over a provider that will not expand nesting makes one request per
// group, so the wide case for that one is a thousand round trips through a test
// server, and every one of them is instrumented here. The number is four times
// the ordinary budget because that is roughly what the instrumentation costs on
// a machine with other work on it, and a bound that failed on a loaded build
// machine would be turned off inside a week.
const budget = 40 * time.Second
