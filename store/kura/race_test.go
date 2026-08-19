//go:build cgo && kura && unix && race

package kura

// raceDetector is whether this binary was built with -race.
//
// The race detector keeps shadow memory for every address the program touches
// and grows the resident set as it does, which is the same measurement the leak
// tests read. So they sit out that pass and run in the plain one, and CI runs
// both.
const raceDetector = true
