package telemetry_test

import (
	"regexp"
	"runtime/debug"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/forkcloser/go-telemetry"
)

// recovered is the attributes of the one record an exporter sends for a
// panic reported with value and stack.
func recovered(t *testing.T, value any, stack []byte) map[string]any {
	t.Helper()

	got := receive(t)

	config := got.config()
	config.Client = got.direct()
	config.Interval = time.Hour
	config.OnError = func(err error) { t.Log(err) }

	exporter := telemetry.New(config)

	exporter.Recovered(value, stack)
	shutdown(t, exporter)

	reported := got.records()
	if len(reported) != 1 {
		t.Fatalf("%d records for one recovered panic", len(reported))
	}

	return mapped(reported[0].GetAttributes())
}

// TestARecoveredPanicIsACrashRecord: with what recover returned as its
// message and the stack of the function that recovered it.
func TestARecoveredPanicIsACrashRecord(t *testing.T) {
	t.Parallel()

	var attrs map[string]any

	func() {
		defer func() { attrs = recovered(t, recover(), debug.Stack()) }()

		panic("recovered on purpose")
	}()

	if attrs["exception.type"] != "panic" || attrs["exception.message"] != "recovered on purpose [recovered]" {
		t.Errorf("reported as %q, %q", attrs["exception.type"], attrs["exception.message"])
	}

	if stack := text(attrs["exception.stacktrace"]); !strings.Contains(stack, "TestARecoveredPanicIsACrashRecord") {
		t.Errorf("the stack does not hold the function that panicked:\n%s", stack)
	}

	if attrs["process.exit.code"] != nil || attrs["crash.signal"] != nil || whole(attrs["process.pid"]) == 0 {
		t.Errorf("exit code %v, signal %v, process %v; want no exit code, no signal, this process",
			attrs["process.exit.code"], attrs["crash.signal"], attrs["process.pid"])
	}
}

// TestALongMessageIsCutAtACharacter: the one line a report says of a crash
// is 1 KiB at most, and whole characters.
func TestALongMessageIsCutAtACharacter(t *testing.T) {
	t.Parallel()

	for name, value := range map[string]string{
		"two bytes a character":   strings.Repeat("é", 2000),
		"three bytes a character": strings.Repeat("€", 2000),
		"four bytes a character":  strings.Repeat("𝄞", 2000),
		"of one byte, then three": "a" + strings.Repeat("€", 2000),
	} {
		message := text(recovered(t, value, debug.Stack())["exception.message"])

		if len(message) > 1<<10 || len(message) < 1<<10-utf8.UTFMax || !utf8.ValidString(message) ||
			!strings.HasPrefix(value, message) {
			t.Errorf(
				"%s: a message of %d bytes, valid: %t, the start of the value: %t; want the 1 KiB before the end of a character",
				name,
				len(message),
				utf8.ValidString(message),
				strings.HasPrefix(value, message),
			)
		}
	}
}

// checkRecovered holds a record of a recovered panic to what every crash
// record is, whatever it was made of.
func checkRecovered(t *testing.T, value string, stack []byte) {
	t.Helper()

	attrs := recovered(t, value, stack)

	if mark := text(attrs["crash.fingerprint"]); !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(mark) {
		t.Errorf("fingerprint %q, want 32 hexadecimal digits", mark)
	}

	if text(attrs["exception.type"]) == "" {
		t.Error("no exception.type")
	}

	if message := text(attrs["exception.message"]); len(message) > 1<<10+len("�") || strings.Contains(message, "\n") {
		t.Errorf(
			"a message of %d bytes, on one line: %t; want 1 KiB at most, on one",
			len(message),
			!strings.Contains(message, "\n"),
		)
	}

	if printed := text(attrs["exception.stacktrace"]); len(printed) > 64<<10+len("�") {
		t.Errorf("a stack of %d bytes, want 64 KiB at most", len(printed))
	}

	// The same again has the same fingerprint.
	if again := recovered(t, value, stack); again["crash.fingerprint"] != attrs["crash.fingerprint"] {
		t.Errorf(
			"the same panic, twice: fingerprints %v and %v",
			attrs["crash.fingerprint"],
			again["crash.fingerprint"],
		)
	}
}

// stacks is stacks no runtime prints, which a program may hand Recovered all
// the same.
func stacks() map[string]string {
	return map[string]string{
		"nothing":                          "",
		"no goroutine":                     "main.main()\n\t/main.go:1 +0x1\n",
		"a goroutine and nothing under it": "goroutine 1 [running]:",
		"a parenthesis that closes none":   "goroutine 1 [running]:\noops)\n)\n",
		"a creator and no goroutine":       "goroutine 1 [running]:\ncreated by main.main\n",
		"a creator in a goroutine":         "goroutine 7 [running]:\nmain.work(...)\n\t/main.go:9\ncreated by main.main in goroutine 1\n",
		"frames left out":                  "goroutine 1 [running]:\nmain.a()\n...additional frames elided...\nmain.z()\n",
		"only the runtime":                 "goroutine 1 [running]:\nruntime.gopark()\nruntime.main()\n",
		"only a panic":                     "goroutine 1 [running]:\npanic({0x1, 0x2})\n",
		"bytes that are no text":           "goroutine 1 [running]:\n\xff\xfe(\x80)\n\t\xc3\n",
		"a line of 100 KiB":                "goroutine 1 [running]:\n" + strings.Repeat("f", 100<<10) + "()\n",
		"blank lines":                      "\n\n\ngoroutine 1 [running]:\n\n\n",
	}
}

// TestRecoveredTakesAnyStack: what a program hands Recovered as a stack is
// its own to choose, and whatever it is makes a crash record.
func TestRecoveredTakesAnyStack(t *testing.T) {
	t.Parallel()

	for name, stack := range stacks() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			checkRecovered(t, "a value\nof two lines, and \xff", []byte(stack))
		})
	}
}

// FuzzRecovered: Recovered makes a crash record of any value and any stack.
func FuzzRecovered(f *testing.F) {
	f.Add("a value", debug.Stack())

	for _, stack := range stacks() {
		f.Add("a value\nof two lines, and \xff", []byte(stack))
	}

	f.Fuzz(checkRecovered)
}

// TestAFingerprintIsOfTheProgramsOwnFrames: the functions of the goroutine
// that panicked, from where it did. Not what stands between the panic and
// the function that recovered it, nor the runtime's own functions under the
// panic, which a traceback shows or not as GOTRACEBACK has it; not the
// arguments, the files, the addresses or the goroutine's number.
func TestAFingerprintIsOfTheProgramsOwnFrames(t *testing.T) {
	t.Parallel()

	fingerprint := func(stack string) string {
		return text(recovered(t, "a fault", []byte(stack))["crash.fingerprint"])
	}

	plain := fingerprint("goroutine 1 [running]:\n" +
		"main.fault(...)\n\t/main.go:7 +0x18\n" +
		"main.main()\n\t/main.go:3 +0x1c\n")

	recoveredInFull := fingerprint("goroutine 18 gp=0x1 m=3 mp=0x2 [running]:\n" +
		"runtime/debug.Stack()\n\t/debug/stack.go:26 +0x64 fp=0x1 sp=0x2 pc=0x3\n" +
		"main.guard.func1()\n\t/main.go:11 +0x40 fp=0x1 sp=0x2 pc=0x3\n" +
		"panic({0x100, 0x200})\n\t/runtime/panic.go:860 +0x12c fp=0x1 sp=0x2 pc=0x3\n" +
		"runtime.panicmem(...)\n\t/runtime/panic.go:330\n" +
		"runtime.sigpanic()\n\t/runtime/signal_unix.go:939 +0x1fc fp=0x1 sp=0x2 pc=0x3\n" +
		"main.fault(0x0)\n\t/elsewhere/main.go:99 +0x18 fp=0x1 sp=0x2 pc=0x3\n" +
		"main.main()\n\t/main.go:3 +0x1c fp=0x1 sp=0x2 pc=0x3\n" +
		"\ngoroutine 2 [force gc (idle)]:\nruntime.gopark()\n")

	elsewhere := fingerprint("goroutine 1 [running]:\n" +
		"main.other(...)\n\t/main.go:7 +0x18\n" +
		"main.main()\n\t/main.go:3 +0x1c\n")

	if plain != recoveredInFull {
		t.Errorf("the same frames, plain and recovered with the runtime's shown: %q and %q", plain, recoveredInFull)
	}

	if plain == elsewhere {
		t.Errorf("other frames have the same fingerprint, %q", plain)
	}

	// A value that reads as a traceback, an error that wraps another
	// program's crash, is the value still.
	stack := "goroutine 1 [running]:\nmain.fault(...)\n\t/main.go:7 +0x18\n"
	quoting := text(
		recovered(t, "the child said:\ngoroutine 9 [running]:\nmain.other()\n\ngoroutine 1:", []byte(stack))["crash.fingerprint"],
	)

	if said := text(recovered(t, "a fault", []byte(stack))["crash.fingerprint"]); quoting != said {
		t.Errorf("the same frames under a value that quotes a traceback: %q, want %q", quoting, said)
	}
}
