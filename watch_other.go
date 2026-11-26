//go:build !unix && !windows

package telemetry

// begin does nothing where a program cannot start another: the program runs
// unwatched.
func begin(Config) error { return nil }

func aside(uintptr) {}

func ending(ended *crash, _ []uintptr) bool { return ended.text != "" }

// given reports that no process here is started in a role.
func given(string, []uintptr) bool { return false }
