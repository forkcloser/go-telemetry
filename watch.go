package telemetry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

const (
	// roleVariable is how Watch tells the process it starts what that
	// process is: the role, then the numbers the role needs, the descriptors
	// or handles it was started with among them. It is a word between the
	// two processes of one program, set by the one and taken out by the
	// other, and the one thing this package reads of the environment: the
	// program's arguments are the program's, and a second process has no
	// other way to be told before its main has run. Set by anything but
	// Watch it is not a role (assigned). Its name says what it is: internal,
	// no part of what the package offers, and free to change.
	roleVariable = "_TELEMETRY_INTERNAL_ROLE"
	// roleWatcher is the process beside a program, which reads its crash
	// output: the number of that, then the program's process.
	roleWatcher = "watcher"
	// roleProgram is the program under a watcher that is the first process:
	// the number of its crash output, then of what tells it to go on.
	roleProgram = "program"
)

// Watch has the program's crashes reported: every end of it the Go runtime
// has something to say about, a panic in any goroutine, a fatal error, a
// fault. A program calls it first in main, before anything else, with the
// Config its exporter gets:
//
//	func main() {
//		config := telemetry.Config{Endpoint: endpoint, Service: "door"}
//		telemetry.Watch(config)
//		…
//	}
//
// A crash is reported by another process, the watcher, since the one that
// crashes cannot: Watch starts the same binary again, with the same arguments
// and environment, and in that second process main runs to this same call.
// Which of the two is the watcher depends on where the program runs.
//
// Everywhere but as the first process of a container, the watcher is the
// second process: in it Watch never returns. It stands aside, in a session of
// its own, holds none of the program's files but its standard error, and
// ends when the program does. The program stays the process that was
// started: its signals, its terminal, its exit status and what started it
// are untouched. After a crash its standard error stays open until the
// report is delivered or given up, [Config.CrashDelivery] at most, and what
// reads it to its end waits that long.
//
// As the first process of a Linux container the program cannot be the one
// that crashes: when the first process ends the kernel kills every other, a
// watcher among them. There the first process is the watcher and the second
// the program, in which Watch returns. The watcher passes on the signals it
// is sent, reaps the processes left to it, keeps none of the files the
// container was started with, and exits as the program did:
// with its exit code, or with 128 and the number of the signal that killed
// it. There it also reports an end by a signal nobody sent on purpose, the
// kernel's kill for memory among them, as Windows' watcher reports an exit
// with the status of a fault.
//
// The watcher must outlive the program to report its crash. Where something
// ends every process with the program, a container whose first process is
// another, a job object, the report is sent only if it is sent by then.
//
// With no endpoint in config Watch does nothing: a program that is not to be
// watched, under a debugger say, calls it with none or does not call it.
// Nor does it keep a program from running: when the second process cannot be
// started the program runs unwatched, and says so on standard error. It
// panics as New does on a Config that is wrong.
func Watch(config Config) {
	switch role, numbers := assigned(); role {
	case roleProgram:
		watched(numbers)

		return
	case roleWatcher:
		//revive:disable-next-line:deep-exit // this process is the watcher, and its whole run is this call
		os.Exit(beside(config, numbers))
	}

	if config.Endpoint == "" {
		return
	}

	// Before anything is started: a Config that is wrong is the program's
	// mistake, to be met at once and not on the day of a crash.
	config.reached()

	if err := begin(config); err != nil {
		fmt.Fprintf(os.Stderr, "telemetry: crashes will not be reported: %v\n", err)
	}
}

// assigned is the role this process was started in by another's Watch, with
// the role's numbers, and none in a process started by anything else.
//
// The variable alone does not make a role: whoever starts a program can set
// one, and a program that took its word would not run. A role is taken only
// when what it names is what Watch gives the process it starts, which no
// environment gives: see given.
func assigned() (string, []uintptr) {
	role, list, _ := strings.Cut(os.Getenv(roleVariable), " ")
	if role == "" {
		return "", nil
	}

	// What this process starts in its turn has no role.
	_ = os.Unsetenv(roleVariable)

	var numbers []uintptr

	for field := range strings.FieldsSeq(list) {
		number, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			return "", nil
		}

		numbers = append(numbers, uintptr(number))
	}

	if !given(role, numbers) {
		return "", nil
	}

	return role, numbers
}

// again is this program once more, in a role.
func again(role string, numbers ...uintptr) (*exec.Cmd, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("telemetry: %w", err)
	}

	fields := []string{role}
	for _, number := range numbers {
		fields = append(fields, strconv.FormatUint(uint64(number), 10))
	}

	// #nosec G204 G702 -- this very program, with its own arguments.
	second := exec.CommandContext(context.Background(), self, os.Args[1:]...)
	second.Args[0] = os.Args[0]

	second.Env = append(os.Environ(), roleVariable+"="+strings.Join(fields, " "))

	return second, nil
}

// crashOutput has the runtime write what it prints of a crash to output, as
// well as to standard error, and closes output: the runtime keeps a
// descriptor of its own, which nothing the program starts inherits.
func crashOutput(output *os.File) error {
	if output == nil {
		return fmt.Errorf("telemetry: %w: no crash output", os.ErrInvalid)
	}

	defer func() { _ = output.Close() }()

	if err := debug.SetCrashOutput(output, debug.CrashOptions{}); err != nil {
		return fmt.Errorf("telemetry: %w", err)
	}

	return nil
}

// collected is what the runtime printed of a crash, read to its end: the
// first of it, as much as a report carries and one byte, which is how the
// report knows to end at a line, and how many bytes came after. The rest is
// read as well, so that the runtime is never kept waiting on a full pipe.
func collected(output io.Reader) (string, int) {
	text, _ := io.ReadAll(io.LimitReader(output, stackBytes+1))
	rest, _ := io.Copy(io.Discard, output)

	return string(text), int(rest)
}

// report sends the record of a crash, within ctx, and says so on standard
// error when the endpoint did not take it.
func report(ctx context.Context, config Config, ended crash) {
	if err := deliver(ctx, config, ended.record()); err != nil {
		fmt.Fprintf(os.Stderr, "telemetry: the report of this crash was not delivered: %v\n", err)
	}
}

// deliver sends one record, again while the endpoint cannot take it for now,
// for as long as config and ctx allow and no longer.
func deliver(ctx context.Context, config Config, record []byte) error {
	base, client := config.reached()

	within := config.CrashDelivery
	if within <= 0 {
		within = DefaultCrashDelivery
	}

	ctx, cancel := context.WithTimeout(ctx, within)
	defer cancel()

	sending := &sender{
		logsURL:  base.JoinPath("v1", "logs").String(),
		headers:  config.Headers,
		client:   client,
		resource: resource(config),
	}

	records := message{}

	records.nested(scopeEnvelopeItems, record)

	body := envelope(sending.resource, records)

	const first, longest = time.Second, 8 * time.Second

	for wait := first; ; wait = min(2*wait, longest) {
		later, err := sending.post(ctx, sending.logsURL, body)
		if err == nil || !later {
			return err
		}

		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return err
		}
	}
}

// watched is Watch in the program under a watcher that is the first process:
// the runtime's crash text goes to the watcher, and the program goes on once
// the watcher has closed its end of what the second number names, which it
// does when the program has the terminal.
func watched(numbers []uintptr) {
	const needs = 2

	if len(numbers) != needs {
		return
	}

	if err := crashOutput(os.NewFile(numbers[0], "crash output")); err != nil {
		fmt.Fprintf(os.Stderr, "telemetry: crashes will not be reported: %v\n", err)
	}

	if held := os.NewFile(numbers[1], "the watcher's word"); held != nil {
		// Its end, when the watcher closes its own, is the word.
		if _, err := held.Read(make([]byte, 1)); err != nil && !errors.Is(err, io.EOF) {
			fmt.Fprintf(os.Stderr, "telemetry: the watcher's word was not heard: %v\n", err)
		}

		_ = held.Close()
	}
}

// beside is Watch in the watcher beside a program: it reads what the runtime
// prints of the program's crash until the program is gone, reports it when
// there is something to report, and returns what the watcher exits with.
func beside(config Config, numbers []uintptr) int {
	const needs = 2

	if len(numbers) < needs || config.Endpoint == "" {
		return 1
	}

	aside(numbers[0])

	output := os.NewFile(numbers[0], "crash output")
	if output == nil {
		return 1
	}

	ended := crash{pid: int(numbers[1])} // #nosec G115 -- a process's number, as the program wrote it
	ended.text, ended.cut = collected(output)
	ended.at = time.Now()

	if ending(&ended, numbers) {
		report(context.Background(), config, ended)
	}

	return 0
}
