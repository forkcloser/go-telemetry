package telemetry

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

// A crash report is a log record like any other, through the same door. What
// makes it one is its attributes:
//
//   - exception.type, exception.message and exception.stacktrace, which are
//     OpenTelemetry's names: what kind of end it was, one line that says it,
//     and what the language's runtime printed, as it printed it;
//   - crash.fingerprint, which is the same for two crashes at the same place
//     in the code, whatever the message, the addresses and the goroutine
//     numbers: what a store counts by. A record that has it is a crash;
//   - crash.signal, when a signal killed the program; process.exit.code, when
//     it exited; process.pid;
//   - crash.stacktrace.cut, how many bytes of the runtime's text were left
//     out, when it was longer than a record carries.
//
// Its severity is FATAL, and its body is the type and the message.
const (
	// stackBytes is how much of the runtime's text a report carries, from
	// its start: the reason and the goroutine that crashed come first.
	stackBytes = 64 << 10
	// messageBytes bounds exception.message, which is one line.
	messageBytes = 1 << 10
	// fingerprintFrames is how many frames a fingerprint is made of at most.
	fingerprintFrames = 64

	severityFatal     = 21
	severityFatalText = "FATAL"

	// kindSignal is the exception.type of a program a signal killed with
	// nothing printed, and kindFatal that of one the Go runtime ended for a
	// fatal error of its own.
	kindSignal = "signal"
	kindFatal  = "fatal error"
	// kindStatus is that of a program that exited with the status of a
	// fault, with nothing printed: what Windows has where Unix has a signal.
	kindStatus = "status"
)

// crash is how a program ended, when it ended badly.
type crash struct {
	at time.Time
	// text is what the runtime printed, from its start, and cut how many
	// bytes of it are not in text.
	text string
	cut  int
	// signal names the signal that killed the program, when one did.
	signal string
	// code is the program's exit code, when exited is set.
	code   int
	exited bool
	pid    int
}

// record writes the crash as a LogRecord message.
func (c crash) record() []byte {
	kind, said := summary(c.text)
	how := c.signal

	switch {
	case c.text != "":
	case c.signal != "":
		kind, said = kindSignal, "killed by "+c.signal
	default:
		how = fmt.Sprintf("%#x", uint32(c.code)) // #nosec G115 -- a Windows exit status, 32 bits as it was read
		kind, said = kindStatus, "exited with "+how
	}

	text, cut := head(c.text, stackBytes)
	cut += c.cut

	written := message{}

	written.fixed(recordTime, nanoseconds(c.at))
	written.fixed(recordObservedTime, nanoseconds(c.at))
	written.uint(recordSeverity, severityFatal)
	written.text(recordSeverityText, severityFatalText)
	written.nested(recordBody, stringValue(kind+": "+said))

	attrs := []slog.Attr{
		slog.String("exception.type", kind),
		slog.String("exception.message", said),
		slog.String("crash.fingerprint", fingerprint(kind, how, frames(c.text))),
	}

	if text != "" {
		attrs = append(attrs, slog.String("exception.stacktrace", text))
	}

	if cut > 0 {
		attrs = append(attrs, slog.Int("crash.stacktrace.cut", cut))
	}

	if c.signal != "" {
		attrs = append(attrs, slog.String("crash.signal", c.signal))
	}

	if c.exited {
		attrs = append(attrs, slog.Int("process.exit.code", c.code))
	}

	if c.pid != 0 {
		attrs = append(attrs, slog.Int("process.pid", c.pid))
	}

	for _, attr := range attrs {
		appendAttr(&written, recordAttributes, attr)
	}

	return written
}

// head is the start of text, limit bytes of it at most, ended at the end of
// a line when there is one to end at, and how many bytes it leaves out.
func head(text string, limit int) (string, int) {
	if len(text) <= limit {
		return text, 0
	}

	end := limit

	if line := strings.LastIndexByte(text[:limit], '\n'); line > 0 {
		end = line + 1
	}

	// Not in the middle of a character: back to where the one that is cut
	// begins, which is three bytes back at most.
	for back := 1; back < utf8.UTFMax && end > 0 && !utf8.RuneStart(text[end]); back++ {
		end--
	}

	return text[:end], len(text) - end
}

// summary reads what kind of end the runtime's text tells, and the one line
// it says of it: "panic" and the value, the name of a signal and what it is.
// The runtime writes that first, before any goroutine.
//
// But for its own fatal errors, a map written by two goroutines, a deadlock,
// memory it cannot have: it prints "fatal error:" and its reason before it
// starts copying what it prints to the crash output, so the text a watcher
// has begins at the goroutine, and the reason is on standard error alone.
// What is left to say of such an end is where it happened.
func summary(text string) (kind, said string) {
	first, _, _ := strings.Cut(strings.TrimLeft(text, "\n"), "\n")
	first = strings.TrimSpace(first)

	if strings.HasPrefix(first, "goroutine ") {
		where := "the Go runtime"
		if functions := frames(text); len(functions) > 0 {
			where = functions[0]
		}

		return kindFatal, "in " + where
	}

	kind, said, found := strings.Cut(first, ": ")
	if !found || strings.ContainsAny(kind, " \t") && kind != kindFatal {
		kind, said = "crash", first
	}

	if len(said) > messageBytes {
		said, _ = head(said, messageBytes)
	}

	return kind, said
}

// frames is the functions on the stack of the first goroutine the runtime
// printed, which is the one that crashed, from where it crashed outwards.
//
// What varies from one run to the next is left out: the arguments, the
// addresses, the files and their lines, the goroutine's number. So is what
// only says how the crash was met: everything up to the runtime's own panic
// function, which is there when the panic was recovered and its stack taken
// from the function that recovered it, and the runtime's functions after it.
// The same fault has the same frames whether a program recovered it or not.
func frames(text string) []string {
	_, stack, found := strings.Cut("\n"+text, "\ngoroutine ")
	if !found {
		return nil
	}

	// The goroutine's own line, then its frames, to the first empty line.
	_, stack, _ = strings.Cut(stack, "\n")
	stack, _, _ = strings.Cut(stack, "\n\n")

	var functions []string

	for line := range strings.SplitSeq(stack, "\n") {
		switch {
		// A file and its line, under the function they are of; frames the
		// runtime left out.
		case line == "", line[0] == '\t', line[0] == ' ', strings.HasPrefix(line, "..."):
			continue
		case strings.HasPrefix(line, "created by "):
			line, _, _ = strings.Cut(line, " in goroutine ")
		// The arguments.
		case strings.HasSuffix(line, ")"):
			if opened := strings.LastIndexByte(line, '('); opened >= 0 {
				line = line[:opened]
			}
		}

		if line == "panic" {
			functions = functions[:0]

			continue
		}

		functions = append(functions, line)
	}

	for len(functions) > 0 && strings.HasPrefix(functions[0], "runtime.") {
		functions = functions[1:]
	}

	return functions[:min(len(functions), fingerprintFrames)]
}

// fingerprint is what two crashes at the same place have in common: the
// kind, how the program ended when it ended with nothing said, a signal's
// name or a status, and the frames. 32 hexadecimal digits of their SHA-256.
func fingerprint(kind, how string, functions []string) string {
	parts := []string{kind}

	if len(functions) == 0 {
		parts = append(parts, how)
	}

	sum := sha256.Sum256([]byte(strings.Join(append(parts, functions...), "\n")))

	const digits = 16

	return hex.EncodeToString(sum[:digits])
}

// Recovered reports a panic the program recovered as the crash it is: the
// same record a watcher makes of a panic that was not recovered, with the
// same fingerprint for the same fault. value is what recover returned, and
// stack what runtime/debug.Stack returned in the function that recovered it.
//
// The program goes on, or ends as it chooses: the record waits in the
// exporter's queue like any other, and Shutdown sends it.
func (e *Exporter) Recovered(value any, stack []byte) {
	if e.sender == nil {
		return
	}

	// As the runtime prints a value: the lines after its first set in, so
	// that none of them reads as a goroutine's.
	said := strings.ReplaceAll(fmt.Sprint(value), "\n", "\n\t")
	text := fmt.Sprintf("panic: %s [recovered]\n\n%s", said, stack)

	e.sender.enqueue(crash{at: time.Now(), text: text, pid: os.Getpid()}.record())
}
