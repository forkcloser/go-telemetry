//go:build linux

package telemetry_test

// The watcher as the first process of a container. A container is, to what
// runs in it, a namespace of processes in which it is number 1: the tests
// start the program as the first process of a new one, which is what a
// container runtime does, where the kernel lets them.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// What a process of a test does that is no program under watch: a way that
// begins as these do is gone into before Watch.
const (
	leavesAChild     = helper + "leaves a child behind"
	endsAfterAMoment = helper + "ends after a moment"
)

// onlyHere is the ways a program ends, or goes on, that only Linux has.
func onlyHere(way string) int {
	switch way {
	case leavesAChild:
		// The child goes on for a moment after its parent has ended.
		child := exec.CommandContext(context.Background(), os.Args[0])

		child.Env = append(os.Environ(), programVariable+"="+endsAfterAMoment)

		if err := child.Start(); err != nil {
			fmt.Fprintln(os.Stdout, err)

			return 1
		}

		fmt.Fprintln(os.Stdout, child.Process.Pid)
	case endsAfterAMoment:
		time.Sleep(200 * time.Millisecond)
	case "takes signals":
		taken := make(chan os.Signal, 8)
		signal.Notify(taken, syscall.SIGUSR1, syscall.SIGINT, syscall.SIGTERM)

		fmt.Fprintln(os.Stdout, "ready")

		for sig := range taken {
			fmt.Fprintf(os.Stdout, "took %v\n", sig)

			if sig == syscall.SIGTERM {
				return 0
			}
		}
	case "writes to the file it was given":
		if _, err := os.NewFile(3, "given").WriteString("written by the program"); err != nil {
			fmt.Fprintln(os.Stdout, err)

			return 1
		}
	case "leaves an orphan":
		return orphaned()
	case "is stopped from its terminal":
		fmt.Fprintln(os.Stdout, "stopping")

		_ = syscall.Kill(os.Getpid(), syscall.SIGTSTP)

		fmt.Fprintln(os.Stdout, "went on")
	}

	return 0
}

// orphaned has a process end before its child, which leaves the child to the
// first process, and says whether the child was waited for once it ended: a
// process that has ended and was not is still there to signal.
func orphaned() int {
	between := exec.CommandContext(context.Background(), os.Args[0])

	between.Env = append(os.Environ(), programVariable+"="+leavesAChild)

	printed, err := between.Output()
	if err != nil {
		fmt.Fprintln(os.Stdout, err)

		return 1
	}

	orphan, err := strconv.Atoi(strings.TrimSpace(string(printed)))
	if err != nil {
		fmt.Fprintln(os.Stdout, err)

		return 1
	}

	for deadline := time.Now().Add(patient / 4); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if errors.Is(syscall.Kill(orphan, 0), syscall.ESRCH) {
			fmt.Fprintln(os.Stdout, "the orphan was waited for")

			return 0
		}
	}

	fmt.Fprintln(os.Stdout, "the orphan was never waited for")

	return 1
}

// waitFor waits until the run has printed text.
func (s *said) waitFor(t *testing.T, text string) {
	t.Helper()

	for deadline := time.Now().Add(patient); !strings.Contains(s.String(), text); {
		if time.Now().After(deadline) {
			t.Fatalf("the program never printed %q:\n%s", text, s.String())
		}

		time.Sleep(time.Millisecond)
	}
}

// asFirst starts again as the first process of a new namespace of processes.
// It skips the test where the kernel does not let this user make one.
func asFirst(t *testing.T, again *exec.Cmd) {
	t.Helper()

	if again.SysProcAttr == nil {
		again.SysProcAttr = &syscall.SysProcAttr{}
	}

	again.SysProcAttr.Cloneflags |= syscall.CLONE_NEWPID

	// Root makes one as it is; anyone else in a namespace of users of their
	// own, where they are root.
	if os.Geteuid() != 0 {
		again.SysProcAttr.Cloneflags |= syscall.CLONE_NEWUSER
		again.SysProcAttr.UidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}}
		again.SysProcAttr.GidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}}
	}

	err := again.Start()

	for _, refused := range []error{syscall.EPERM, syscall.EACCES, syscall.EINVAL, syscall.ENOSPC, syscall.ENOSYS} {
		if errors.Is(err, refused) {
			t.Skipf("no namespace of processes for this user here: %v", err)
		}
	}

	if err != nil {
		t.Fatal(err)
	}
}

func firstProgram(t *testing.T, way, endpoint string) watchedRun {
	t.Helper()

	again, output := prepare(t, way, endpoint)

	asFirst(t, again)

	return finish(t, again, output)
}

// TestTheFirstProcessOfAContainerIsTheWatcher: there the program is another
// process than the one that was started, which exits as the program did:
// with its exit code, or 128 and the signal's number. An end that is no
// crash is not reported.
func TestTheFirstProcessOfAContainerIsTheWatcher(t *testing.T) {
	t.Parallel()

	for way, status := range map[string]int{"ends": 0, "exits with 3": 3, "is terminated": 128 + int(syscall.SIGTERM)} {
		got := receive(t)

		ran := firstProgram(t, way, got.URL)

		// The watcher has nothing to say of a run that went well: a line of
		// its own would be of something that did not.
		if strings.Contains(ran.output, "telemetry:") {
			t.Errorf("a program that %s: the watcher said\n%s", way, ran.output)
		}

		if ran.status != status || ran.programPID(t) == 1 {
			t.Errorf("a program that %s: %s, the program is process %d; want exit status %d, and not the first\n%s",
				way, ran.ended, ran.programPID(t), status, ran.output)
		}

		if reported := got.records(); len(reported) != 0 {
			t.Errorf("a program that %s: %d records reported, want none", way, len(reported))
		}
	}
}

// TestTheFirstProcessReportsACrash: what the runtime had something to say
// about, with what the program exited with; and an end by a signal nobody
// sent on purpose, the kernel's kill among them, which nothing is printed of.
func TestTheFirstProcessReportsACrash(t *testing.T) {
	t.Parallel()

	got := receive(t)

	ran := firstProgram(t, "panics in another goroutine", got.URL)
	_, attrs := crashed(t, got, ran)

	stack := text(attrs["exception.stacktrace"])

	if ran.status != 2 || attrs["exception.type"] != "panic" || attrs["process.exit.code"] != int64(2) ||
		attrs["process.pid"] != int64(ran.programPID(t)) || !strings.Contains(stack, "telemetry_test.program.func1") {
		t.Errorf("a panic: %s, reported as %v with exit code %v of process %v\n%s",
			ran.ended, attrs["exception.type"], attrs["process.exit.code"], attrs["process.pid"], stack)
	}

	killed := receive(t)

	ran = firstProgram(t, "is killed", killed.URL)
	record, attrs := crashed(t, killed, ran)

	if ran.status != 128+int(syscall.SIGKILL) || record.GetBody().GetStringValue() != "signal: killed by SIGKILL" {
		t.Errorf("a kill: %s, reported as %q; want exit status 137", ran.ended, record.GetBody().GetStringValue())
	}

	if attrs["exception.type"] != "signal" || attrs["crash.signal"] != "SIGKILL" ||
		attrs["process.exit.code"] != nil || attrs["exception.stacktrace"] != nil {
		t.Errorf("a kill: type %v, signal %v, exit code %v, stack %v",
			attrs["exception.type"], attrs["crash.signal"], attrs["process.exit.code"], attrs["exception.stacktrace"])
	}
}

// TestAStopAfterACrashEndsTheReport: once the program has ended the first
// process is all there is of the container, and a signal that stops a
// program stops it: it gives up the report it was at, says so, and exits as
// the program did.
func TestAStopAfterACrashEndsTheReport(t *testing.T) {
	t.Parallel()

	got := receive(t)
	release := got.holding()

	defer release()

	// Far longer than the test waits.
	again, output := prepare(t, "faults", got.URL, deliveryVariable+"=1h")

	asFirst(t, again)

	// The watcher is at its report.
	got.waitAsked(t)

	if err := again.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}

	ran := finish(t, again, output)

	if ran.status != 2 || !strings.Contains(ran.output, "the report of this crash was not delivered") {
		t.Errorf("%s, want exit status 2 and the report given up\n%s", ran.ended, ran.output)
	}
}

// TestTheFirstProcessPassesItsSignalsOn: what is sent to the first process
// is the program's.
func TestTheFirstProcessPassesItsSignalsOn(t *testing.T) {
	t.Parallel()

	got := receive(t)

	again, output := prepare(t, "takes signals", got.URL)

	asFirst(t, again)

	output.waitFor(t, "ready")

	for _, sig := range []syscall.Signal{syscall.SIGUSR1, syscall.SIGINT, syscall.SIGTERM} {
		if err := again.Process.Signal(sig); err != nil {
			t.Fatal(err)
		}

		output.waitFor(t, "took "+sig.String())
	}

	if ran := finish(t, again, output); ran.status != 0 || len(got.records()) != 0 {
		t.Errorf(
			"a program stopped by a signal it handles: %s, %d records; want exit status 0 and none",
			ran.ended,
			len(got.records()),
		)
	}
}

// TestTheProgramHasTheFilesTheFirstProcessWasGiven: under the numbers they
// were given under.
func TestTheProgramHasTheFilesTheFirstProcessWasGiven(t *testing.T) {
	t.Parallel()

	got := receive(t)

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = reader.Close() }()

	again, output := prepare(t, "writes to the file it was given", got.URL)
	again.ExtraFiles = []*os.File{writer}

	asFirst(t, again)

	_ = writer.Close()

	ran := finish(t, again, output)

	_ = reader.SetReadDeadline(time.Now().Add(patient))

	var written [64]byte

	count, _ := reader.Read(written[:])

	if ran.status != 0 || string(written[:count]) != "written by the program" {
		t.Errorf("%s; the file holds %q\n%s", ran.ended, written[:count], ran.output)
	}
}

// TestTheFirstProcessHoldsNoneOfTheProgramsFiles: what the container was
// started with, a socket to listen on, and the program's standard output,
// are the program's alone: once it has crashed nothing holds them, while the
// first process is still at its report.
func TestTheFirstProcessHoldsNoneOfTheProgramsFiles(t *testing.T) {
	t.Parallel()

	got := receive(t)
	release := got.holding()

	defer release()

	// Far longer than the test waits: a file the watcher held would stay
	// open all that time.
	again, output := prepare(t, "faults", got.URL, deliveryVariable+"=1h")

	// Two files the container is started with, and the program's standard
	// output.
	var given, passed [3]*os.File

	for at := range given {
		reader, writer, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}

		t.Cleanup(func() { _ = reader.Close() })
		t.Cleanup(func() { _ = writer.Close() })

		given[at], passed[at] = reader, writer
	}

	again.ExtraFiles, again.Stdout = passed[:2], passed[2]

	asFirst(t, again)

	for at, writer := range passed {
		_ = writer.Close()

		// The pipe ends when its last writer is gone.
		_ = given[at].SetReadDeadline(time.Now().Add(patient))

		if _, err := io.ReadAll(given[at]); err != nil {
			t.Errorf("file %d of the program's is still held after its crash: %v", at, err)
		}
	}

	got.waitAsked(t)
	release()

	if ran := finish(t, again, output); ran.status != 2 || len(got.records()) != 1 {
		t.Errorf(
			"%s, %d records; want exit status 2 and the one of the crash\n%s",
			ran.ended,
			len(got.records()),
			ran.output,
		)
	}
}

// TestTheFirstProcessWaitsForTheProcessesLeftToIt: a process whose parent
// ended before it is the first process's to wait for.
func TestTheFirstProcessWaitsForTheProcessesLeftToIt(t *testing.T) {
	t.Parallel()

	got := receive(t)

	if ran := firstProgram(
		t,
		"leaves an orphan",
		got.URL,
	); ran.status != 0 ||
		!strings.Contains(ran.output, "the orphan was waited for") {
		t.Errorf("%s\n%s", ran.ended, ran.output)
	}
}

// TestAStopFromTheTerminalDoesNotHold: nothing stops the first process of a
// container from its terminal, and the program under it goes on as well.
func TestAStopFromTheTerminalDoesNotHold(t *testing.T) {
	t.Parallel()

	got := receive(t)

	if ran := firstProgram(
		t,
		"is stopped from its terminal",
		got.URL,
	); ran.status != 0 ||
		!strings.Contains(ran.output, "went on") {
		t.Errorf("%s\n%s", ran.ended, ran.output)
	}
}

// terminal opens a pseudo-terminal: the side a program has as its terminal,
// and the side that plays the keyboard and the screen.
func terminal(t *testing.T) (program, keyboard *os.File) {
	t.Helper()

	keyboard, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no pseudo-terminal here: %v", err)
	}

	t.Cleanup(func() { _ = keyboard.Close() })

	var number, unlocked uint32

	for request, into := range map[uintptr]*uint32{syscall.TIOCSPTLCK: &unlocked, syscall.TIOCGPTN: &number} {
		if _, _, failed := syscall.Syscall(
			syscall.SYS_IOCTL,
			keyboard.Fd(),
			request,
			uintptr(unsafe.Pointer(into)),
		); failed != 0 {
			t.Skipf("no pseudo-terminal here: %v", failed)
		}
	}

	program, err = os.OpenFile("/dev/pts/"+strconv.Itoa(int(number)), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pseudo-terminal here: %v", err)
	}

	t.Cleanup(func() { _ = program.Close() })

	return program, keyboard
}

// TestTheTerminalIsThePrograms: an interrupt typed at the terminal of a
// container reaches the program, once, as one sent to the first process
// does.
func TestTheTerminalIsThePrograms(t *testing.T) {
	t.Parallel()

	got := receive(t)

	side, keyboard := terminal(t)

	again, output := prepare(t, "takes signals", got.URL)
	again.Stdin = side
	again.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}

	asFirst(t, again)

	output.waitFor(t, "ready")

	// Ctrl-C.
	if _, err := keyboard.Write([]byte{3}); err != nil {
		t.Fatal(err)
	}

	output.waitFor(t, "took interrupt")

	// A signal after it: once the program has taken this one, every
	// interrupt the terminal sent has been taken as well.
	if err := again.Process.Signal(syscall.SIGUSR1); err != nil {
		t.Fatal(err)
	}

	output.waitFor(t, "took user defined signal 1")

	if err := again.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}

	ran := finish(t, again, output)

	if took := strings.Count(ran.output, "took interrupt"); ran.status != 0 || took != 1 {
		t.Errorf("%s; the program took %d interrupts for one Ctrl-C, want 1\n%s", ran.ended, took, ran.output)
	}
}
