package telemetry_test

// The watcher is tested as it is used: this test binary is run again as a
// program whose main calls Watch first and then ends in one of several ways,
// and what the stand-in endpoint received is read once the run is over. A
// run is over when the program's standard error has been closed by every
// process that holds it, the watcher among them.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	logs "go.opentelemetry.io/proto/otlp/logs/v1"

	"github.com/forkcloser/go-xhttp"

	"github.com/forkcloser/go-telemetry"
)

const (
	// What a test tells the program it starts, which is this binary: the
	// way it is to end, where it is to report, and how long its watcher may
	// take.
	programVariable  = "TELEMETRY_TEST_PROGRAM"
	endpointVariable = "TELEMETRY_TEST_ENDPOINT"
	deliveryVariable = "TELEMETRY_TEST_DELIVERY"

	// roleVariable is the library's own, internal: the word from Watch to
	// the process it starts, which a test sets to see it is not taken from
	// anyone else.
	roleVariable = "_TELEMETRY_INTERNAL_ROLE"

	// helper begins the ways that are not a program under watch: a process a
	// test needs, which calls no Watch.
	helper = "helper: "
)

func TestMain(m *testing.M) {
	if way := os.Getenv(programVariable); way != "" {
		os.Exit(program(way))
	}

	m.Run()
}

// program is the main of the program under watch, and returns what it exits
// with.
func program(way string) int {
	if strings.HasPrefix(way, helper) {
		return otherwise(way)
	}

	config := telemetry.Config{
		Endpoint: os.Getenv(endpointVariable),
		Client:   xhttp.NewClient(xhttp.Config{Cleartext: true}),
		Service:  "watched",
		Version:  "1.2.3",
	}

	if within, err := time.ParseDuration(os.Getenv(deliveryVariable)); err == nil {
		config.CrashDelivery = within
	}

	telemetry.Watch(config)

	fmt.Fprintf(os.Stdout, "the program is %d\n", os.Getpid())

	switch way {
	case "ends":
	case "exits with 3":
		return 3
	case "panics in another goroutine":
		go func() { panic("oops, in another goroutine") }()

		time.Sleep(patient)
	case "faults":
		guarded(nil)
	case "recovers a fault":
		exporter := telemetry.New(config)

		guarded(exporter)

		_ = exporter.Shutdown(context.Background())

		return 1
	case "writes a map from two goroutines":
		racing()
	case "panics with many goroutines":
		debug.SetTraceback("all")

		var waiting sync.WaitGroup

		for range 3000 {
			waiting.Go(func() { time.Sleep(patient) })
		}

		time.Sleep(100 * time.Millisecond)

		panic("with many goroutines")
	default:
		// The ways only some platforms have.
		return otherwise(way)
	}

	return 0
}

type shelf struct{ books []string }

// fault is a fault at the bottom of a few calls.
func fault(depth int) int {
	if depth == 0 {
		// One that is not there.
		return len(map[string]*shelf{}["none"].books)
	}

	return fault(depth-1) + 1
}

// guarded runs into the fault, and reports it through exporter when there is
// one; with none the panic goes on, and ends the program.
func guarded(exporter *telemetry.Exporter) {
	defer func() {
		if exporter == nil {
			return
		}

		if recovered := recover(); recovered != nil {
			exporter.Recovered(recovered, debug.Stack())
		}
	}()

	_ = fault(3)
}

func racing() {
	shared := map[int]int{}

	for range 8 {
		go func() {
			for at := 0; ; at++ {
				shared[at%16] = at
			}
		}()
	}

	time.Sleep(patient)
}

// said is what a run printed so far, read while it is still printing.
type said struct {
	mu      sync.Mutex
	printed bytes.Buffer
}

func (s *said) Write(part []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.printed.Write(part)
}

func (s *said) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.printed.String()
}

// watchedRun is one run of the program.
type watchedRun struct {
	// first is the process the test started, status its exit code, and ended
	// how it ended, as the standard library writes it.
	first  int
	status int
	ended  string
	output string
}

// programPID is the process the program's main went on in.
func (r watchedRun) programPID(t *testing.T) int {
	t.Helper()

	found := regexp.MustCompile(`the program is (\d+)`).FindStringSubmatch(r.output)
	if found == nil {
		t.Fatalf("the program did not say which process it is:\n%s", r.output)
	}

	pid, _ := strconv.Atoi(found[1])

	return pid
}

// prepare is the program in one of its ways, not started, with the endpoint
// and what else its environment is to hold.
func prepare(t *testing.T, way, endpoint string, environment ...string) (*exec.Cmd, *said) {
	t.Helper()

	output := &said{}

	again := exec.CommandContext(t.Context(), os.Args[0])

	again.Env = append(os.Environ(),
		programVariable+"="+way,
		endpointVariable+"="+endpoint,
	)
	again.Env = append(again.Env, environment...)
	again.Stdout, again.Stderr = output, output

	return again, output
}

func start(t *testing.T, way, endpoint string, environment ...string) (*exec.Cmd, *said) {
	t.Helper()

	again, output := prepare(t, way, endpoint, environment...)

	if err := again.Start(); err != nil {
		t.Fatal(err)
	}

	return again, output
}

func finish(t *testing.T, again *exec.Cmd, output *said) watchedRun {
	t.Helper()

	err := again.Wait()

	if _, exited := errors.AsType[*exec.ExitError](err); err != nil && !exited {
		t.Fatal(err)
	}

	return watchedRun{
		first:  again.Process.Pid,
		status: again.ProcessState.ExitCode(),
		ended:  again.ProcessState.String(),
		output: output.String(),
	}
}

func watchedProgram(t *testing.T, way, endpoint string, environment ...string) watchedRun {
	t.Helper()

	again, output := start(t, way, endpoint, environment...)

	return finish(t, again, output)
}

// TestAWatchedProgramIsTheProcessThatWasStarted: beside its watcher a program
// runs in the process that was started and ends with its own status, and an
// end that is no crash is not reported.
func TestAWatchedProgramIsTheProcessThatWasStarted(t *testing.T) {
	t.Parallel()

	for way, status := range map[string]int{"ends": 0, "exits with 3": 3} {
		got := receive(t)

		ran := watchedProgram(t, way, got.URL)

		if ran.status != status || ran.programPID(t) != ran.first {
			t.Errorf("a program that %s: status %d, in the process that was started: %t; want %d, in that one\n%s",
				way, ran.status, ran.programPID(t) == ran.first, status, ran.output)
		}

		if reported := got.records(); len(reported) != 0 {
			t.Errorf("a program that %s: %d records reported, want none", way, len(reported))
		}
	}
}

// TestWithNoEndpointWatchDoesNothing: the program runs as it would without
// the call, ends as it ends, and the library has nothing to say.
func TestWithNoEndpointWatchDoesNothing(t *testing.T) {
	t.Parallel()

	for way, status := range map[string]int{"ends": 0, "exits with 3": 3, "panics in another goroutine": 2} {
		ran := watchedProgram(t, way, "")

		if ran.status != status || ran.programPID(t) != ran.first || strings.Contains(ran.output, "telemetry:") {
			t.Errorf("a program that %s, with no endpoint: status %d, want %d\n%s", way, ran.status, status, ran.output)
		}
	}
}

// TestARoleIsTakenFromWatchAlone: the variable Watch sets for the process it
// starts, set by whoever starts the program, is no role: the program runs,
// is watched as it would be, and the library has nothing to say.
func TestARoleIsTakenFromWatchAlone(t *testing.T) {
	t.Parallel()

	for _, said := range []string{
		"watcher", "watcher 3 1", "watcher 0 1", "watcher 3 " + strconv.Itoa(os.Getpid()), "watcher 3 2 4",
		"program", "program 7 8", "program 0 1", "nonsense", " ", "watcher x y",
	} {
		got := receive(t)

		ran := watchedProgram(t, "exits with 3", got.URL, roleVariable+"="+said)

		if ran.status != 3 || ran.programPID(t) != ran.first || strings.Contains(ran.output, "telemetry:") {
			t.Errorf(
				"started with the role %q: status %d, want the program to run and exit with 3\n%s",
				said,
				ran.status,
				ran.output,
			)
		}
	}

	// And it is watched: a crash under such a word is reported.
	for _, said := range []string{"watcher 3 1", "program 7 8"} {
		got := receive(t)

		ran := watchedProgram(t, "faults", got.URL, roleVariable+"="+said)
		if reported := got.records(); ran.status != 2 || len(reported) != 1 {
			t.Errorf("started with the role %q, a crash: status %d, %d records; want 2 and 1\n%s",
				said, ran.status, len(reported), ran.output)
		}
	}
}

// crashed is the one record a run reported, and its attributes.
func crashed(t *testing.T, got *receiver, ran watchedRun) (*logs.LogRecord, map[string]any) {
	t.Helper()

	reported := got.records()
	if len(reported) != 1 {
		t.Fatalf("%d records reported, want the one of the crash\n%s", len(reported), ran.output)
	}

	return reported[0], mapped(reported[0].GetAttributes())
}

// TestACrashIsReported: every end the Go runtime has something to say about,
// in whatever goroutine, is one record: FATAL, with what kind of end it was,
// the line that says it, what the runtime printed and the program's process.
// The program ends as it would unwatched.
func TestACrashIsReported(t *testing.T) {
	t.Parallel()

	for way, want := range map[string]struct {
		kind, message string
		// in is a function the stack holds.
		in string
	}{
		"panics in another goroutine": {
			kind: "panic", message: "oops, in another goroutine", in: "telemetry_test.program.func1",
		},
		"faults": {
			kind: "panic", message: "runtime error: invalid memory address or nil pointer dereference",
			in: "telemetry_test.fault",
		},
		"writes a map from two goroutines": {
			// The runtime's reason is on standard error alone.
			kind: "fatal error", message: "in internal/runtime/maps.fatal", in: "telemetry_test.racing.func1",
		},
	} {
		got := receive(t)

		ran := watchedProgram(t, way, got.URL)
		record, attrs := crashed(t, got, ran)

		if ran.status != 2 || ran.programPID(t) != ran.first {
			t.Errorf("a program that %s: %s, in the process that was started: %t; want exit status 2, in that one",
				way, ran.ended, ran.programPID(t) == ran.first)
		}

		if record.GetSeverityNumber() != logs.SeverityNumber_SEVERITY_NUMBER_FATAL ||
			record.GetSeverityText() != "FATAL" ||
			record.GetBody().GetStringValue() != want.kind+": "+want.message ||
			record.GetTimeUnixNano() == 0 {
			t.Errorf("a program that %s: severity %v %q, body %q", way,
				record.GetSeverityNumber(), record.GetSeverityText(), record.GetBody().GetStringValue())
		}

		if attrs["exception.type"] != want.kind || attrs["exception.message"] != want.message {
			t.Errorf("a program that %s: %q, %q; want %q, %q", way,
				attrs["exception.type"], attrs["exception.message"], want.kind, want.message)
		}

		if attrs["process.pid"] != int64(ran.first) {
			t.Errorf("a program that %s: process.pid %v, want the program's %d", way, attrs["process.pid"], ran.first)
		}

		// What the program exited with is known where the watcher can read
		// it: on Windows, of a program it is beside.
		var code any
		if runtime.GOOS == "windows" {
			code = int64(ran.status)
		}

		if attrs["process.exit.code"] != code || attrs["crash.signal"] != nil {
			t.Errorf("a program that %s: exit code %v, signal %v; want %v, none",
				way, attrs["process.exit.code"], attrs["crash.signal"], code)
		}

		stack := text(attrs["exception.stacktrace"])

		switch {
		case !strings.Contains(stack, want.in):
			t.Errorf("a program that %s: a stack without %s:\n%s", way, want.in, stack)
		// What the watcher reports is what the runtime printed, whole.
		case !strings.Contains(strings.ReplaceAll(ran.output, "\r\n", "\n"), stack):
			t.Errorf(
				"a program that %s: the stack reported is not what the runtime printed:\n%s\n\n%s",
				way,
				stack,
				ran.output,
			)
		}

		if mark := text(attrs["crash.fingerprint"]); !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(mark) {
			t.Errorf("a program that %s: fingerprint %q, want 32 hexadecimal digits", way, mark)
		}

		if attrs["crash.stacktrace.cut"] != nil {
			t.Errorf("a program that %s: %v bytes cut from a short stack", way, attrs["crash.stacktrace.cut"])
		}
	}
}

// TestTheSameFaultHasTheSameFingerprint: across two runs, where the addresses
// and the goroutines differ; and whether the program recovered the panic and
// reported it itself or was ended by it. Another fault has another.
func TestTheSameFaultHasTheSameFingerprint(t *testing.T) {
	t.Parallel()

	fingerprint := func(way string) string {
		got := receive(t)

		ran := watchedProgram(t, way, got.URL)
		_, attrs := crashed(t, got, ran)

		return text(attrs["crash.fingerprint"])
	}

	first, second := fingerprint("faults"), fingerprint("faults")
	recovered := fingerprint("recovers a fault")
	other := fingerprint("panics in another goroutine")

	if first == "" || first != second {
		t.Errorf("the same fault, twice: %q and %q", first, second)
	}

	if recovered != first {
		t.Errorf("the same fault, recovered and reported by the program: %q, want %q", recovered, first)
	}

	if other == first {
		t.Errorf("another fault has the same fingerprint, %q", other)
	}
}

// TestALongStackIsCutAtItsEnd: a record carries the first 64 KiB of what the
// runtime printed, which holds the reason and the goroutine that crashed,
// and says how much it left out.
func TestALongStackIsCutAtItsEnd(t *testing.T) {
	t.Parallel()

	got := receive(t)

	ran := watchedProgram(t, "panics with many goroutines", got.URL)
	_, attrs := crashed(t, got, ran)

	stack := text(attrs["exception.stacktrace"])
	cut := whole(attrs["crash.stacktrace.cut"])

	_, printed, found := strings.Cut(strings.ReplaceAll(ran.output, "\r\n", "\n"), "panic: with many goroutines")
	if !found {
		t.Fatalf("the program did not panic:\n%s", ran.output)
	}

	printed = "panic: with many goroutines" + printed

	switch {
	case len(stack) > 64<<10 || len(stack) < 60<<10 || !strings.HasSuffix(stack, "\n"):
		t.Errorf("the stack reported is %d bytes, want the 64 KiB before the end of a line", len(stack))
	case !strings.HasPrefix(printed, stack):
		t.Error("the stack reported is not the start of what the runtime printed")
	case int(cut) != len(printed)-len(stack):
		t.Errorf(
			"%d bytes said to be cut, of the %d printed past the %d reported",
			cut,
			len(printed)-len(stack),
			len(stack),
		)
	case attrs["exception.message"] != "with many goroutines":
		t.Errorf("the message of a cut report is %q", attrs["exception.message"])
	}
}

// TestAReportTheEndpointDoesNotTake: one the endpoint cannot take for now is
// sent again until it does, within the time the watcher is given; one it
// cannot take in that time, or refuses, is given up and said on standard
// error.
func TestAReportTheEndpointDoesNotTake(t *testing.T) {
	t.Parallel()

	const notDelivered = "the report of this crash was not delivered"

	back := receive(t)
	back.setAnswer(http.StatusServiceUnavailable)

	again, output := start(t, "faults", back.URL)

	// The watcher's first try has been refused; the endpoint is back for
	// its second.
	back.waitAsked(t)
	back.setAnswer(0)

	if ran := finish(
		t,
		again,
		output,
	); ran.status != 2 || len(back.records()) != 1 ||
		strings.Contains(ran.output, notDelivered) {
		t.Errorf(
			"an endpoint back after a moment: status %d, %d records\n%s",
			ran.status,
			len(back.records()),
			ran.output,
		)
	}

	for name, answer := range map[string]int{
		"an endpoint away for longer than the watcher waits": http.StatusServiceUnavailable,
		"an endpoint that refuses the report":                http.StatusBadRequest,
	} {
		away := receive(t)
		away.setAnswer(answer)

		started := time.Now()

		ran := watchedProgram(t, "faults", away.URL, deliveryVariable+"=1500ms")

		if ran.status != 2 || !strings.Contains(ran.output, notDelivered) || len(away.records()) != 0 {
			t.Errorf(
				"%s: status %d, said so: %t\n%s",
				name,
				ran.status,
				strings.Contains(ran.output, notDelivered),
				ran.output,
			)
		}

		if took := time.Since(started); took > patient/2 {
			t.Errorf("%s: the watcher went on for %v, given 1.5 s", name, took)
		}
	}
}
