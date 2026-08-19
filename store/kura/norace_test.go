//go:build cgo && kura && unix && !race

package kura

// raceDetector is whether this binary was built with -race. See race_test.go.
const raceDetector = false
