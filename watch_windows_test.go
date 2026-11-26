//go:build windows

package telemetry_test

import "testing"

const (
	// accessViolation is STATUS_ACCESS_VIOLATION, what a process Windows
	// ended for a fault exits with; and interrupted STATUS_CONTROL_C_EXIT,
	// what one ended by Ctrl-C exits with.
	accessViolation = 0xC0000005
	interrupted     = 0xC000013A
)

// otherwise is the ways a program ends that only Windows has.
func otherwise(way string) int {
	switch way {
	case "exits with the status of a fault":
		return accessViolation
	case "exits with the status of an interrupt":
		return interrupted
	}

	return 0
}

// TestAnExitWithTheStatusOfAFaultIsReported: a program Windows ended for a
// fault, with nothing printed by the Go runtime, is reported with the status
// it exited with; one ended by Ctrl-C is not.
func TestAnExitWithTheStatusOfAFaultIsReported(t *testing.T) {
	t.Parallel()

	got := receive(t)

	ran := watchedProgram(t, "exits with the status of a fault", got.URL)
	record, attrs := crashed(t, got, ran)

	if ran.status != accessViolation || attrs["process.exit.code"] != int64(accessViolation) {
		t.Errorf(
			"the program exited with %#x, reported as %v; want %#x",
			ran.status,
			attrs["process.exit.code"],
			accessViolation,
		)
	}

	if attrs["exception.type"] != "status" || attrs["exception.message"] != "exited with 0xc0000005" ||
		record.GetBody().GetStringValue() != "status: exited with 0xc0000005" || attrs["exception.stacktrace"] != nil {
		t.Errorf(
			"reported as %q, %q, with stack %v",
			attrs["exception.type"],
			attrs["exception.message"],
			attrs["exception.stacktrace"],
		)
	}

	stopped := receive(t)

	if ran = watchedProgram(t, "exits with the status of an interrupt", stopped.URL); len(stopped.records()) != 0 {
		t.Errorf("a program ended by Ctrl-C: %d records, want none\n%s", len(stopped.records()), ran.output)
	}
}
