//go:build unix

package telemetry_test

import (
	"io"
	"net/http"
	"os"
	"syscall"
	"testing"
	"time"
)

// otherwise is the ways a program ends that only Unix has.
func otherwise(way string) int {
	switch way {
	case "is killed":
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)

		time.Sleep(patient)
	case "is terminated":
		_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)

		time.Sleep(patient)
	default:
		return onlyHere(way)
	}

	return 0
}

// TestAnEndByASignalIsNotSeenFromBesideTheProgram: the watcher beside a
// program is not its parent, and is not told how it ended: an end the
// runtime printed nothing of is not reported, and the watcher ends with the
// program all the same.
func TestAnEndByASignalIsNotSeenFromBesideTheProgram(t *testing.T) {
	t.Parallel()

	for way, ended := range map[string]string{"is killed": "signal: killed", "is terminated": "signal: terminated"} {
		got := receive(t)

		ran := watchedProgram(t, way, got.URL)

		if ran.ended != ended || len(got.records()) != 0 {
			t.Errorf("a program that %s: %s, %d records; want %s and none", way, ran.ended, len(got.records()), ended)
		}
	}
}

// TestTheWatcherHoldsNoneOfTheProgramsFiles: what a program was started
// with past its standard files, a socket it was handed to listen on, is
// closed in its watcher: once the program has crashed nothing holds it,
// while the watcher is still at its report.
func TestTheWatcherHoldsNoneOfTheProgramsFiles(t *testing.T) {
	t.Parallel()

	got := receive(t)
	release := got.holding()

	defer release()

	// Far longer than the test waits: a file the watcher held would stay
	// open all that time.
	again, output := prepare(t, "faults", got.URL, deliveryVariable+"=1h")

	var given [2]*os.File

	for at := range given {
		reader, writer, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}

		t.Cleanup(func() { _ = reader.Close() })

		given[at] = reader

		again.ExtraFiles = append(again.ExtraFiles, writer)
	}

	if err := again.Start(); err != nil {
		t.Fatal(err)
	}

	for at, writer := range again.ExtraFiles {
		_ = writer.Close()

		// The pipe ends when its last writer is gone.
		_ = given[at].SetReadDeadline(time.Now().Add(patient))

		if _, err := io.ReadAll(given[at]); err != nil {
			t.Errorf("the program's file %d is still held after its crash: %v", 3+at, err)
		}
	}

	got.waitAsked(t)
	release()

	if ran := finish(t, again, output); ran.status != 2 || len(got.records()) != 1 {
		t.Errorf(
			"status %d, %d records; want 2 and the one of the crash\n%s",
			ran.status,
			len(got.records()),
			ran.output,
		)
	}
}

// TestTheWatcherIsNotOfTheProgramsGroup: what is sent to the program's
// process group, a terminal's hangup, a supervisor's kill, does not reach the
// watcher, which goes on to deliver its report.
func TestTheWatcherIsNotOfTheProgramsGroup(t *testing.T) {
	t.Parallel()

	// An endpoint that takes nothing yet: a watcher that is killed has
	// delivered nothing.
	got := receive(t)
	got.setAnswer(http.StatusServiceUnavailable)

	again, output := prepare(t, "faults", got.URL)
	again.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := again.Start(); err != nil {
		t.Fatal(err)
	}

	// The watcher is at its report: the program has crashed.
	got.waitAsked(t)

	// The group is the program's, which has ended and has not been waited
	// for: there may be no one left in it to signal.
	_ = syscall.Kill(-again.Process.Pid, syscall.SIGKILL)

	got.setAnswer(0)

	if ran := finish(t, again, output); len(got.records()) != 1 {
		t.Errorf(
			"%d records after the program's group was killed, want the one of the crash\n%s",
			len(got.records()),
			ran.output,
		)
	}
}
