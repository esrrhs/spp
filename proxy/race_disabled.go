//go:build !race

package proxy

// See race_enabled.go: full worker counts when the race detector is off.
const sppRaceWorkers = 20
