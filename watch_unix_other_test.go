//go:build unix && !linux

package telemetry_test

// onlyHere is the ways a program ends that only this platform has: none.
func onlyHere(string) int { return 0 }
