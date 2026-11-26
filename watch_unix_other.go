//go:build unix && !linux

package telemetry

// first reports whether this process is the first of a container, which
// only Linux has.
func first() bool { return false }

// underFirst reports whether this process is the child of the first process
// of a container, which only Linux has.
func underFirst() bool { return false }

// above is the watcher as the first process of a container, which only
// Linux has.
func above(Config) (int, error) { return 0, nil }
