//go:build unix

package telemetry

import (
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// standard is how many descriptors a process has before any it was given:
// its input, its output and its errors.
const standard = 3

// begin starts the watcher of this program: this process, when it is the
// first of a container, and a second one beside it anywhere else.
func begin(config Config) error {
	if first() {
		status, err := above(config)
		if err != nil {
			return err
		}

		//revive:disable-next-line:deep-exit // this process is the watcher, and its whole run is this call
		os.Exit(status)
	}

	reader, writer, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("telemetry: %w", err)
	}

	defer func() { _ = reader.Close() }()

	second, err := again(roleWatcher, standard, uintptr(os.Getpid()))
	if err != nil {
		_ = writer.Close()

		return err
	}

	second.Stderr = os.Stderr
	second.ExtraFiles = []*os.File{reader}
	// A session of its own: what a terminal or a shell sends the program's
	// group, an interrupt, a hangup, is not sent to the watcher.
	second.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err = second.Start(); err != nil {
		_ = writer.Close()

		return fmt.Errorf("telemetry: %w", err)
	}

	// A watcher that ends before the program leaves it nothing to wait for.
	go func() { _ = second.Wait() }()

	return crashOutput(writer)
}

// aside sets the watcher beside a program apart from it. The watcher ends
// when the program does and at no one's word before, so that a stop meant
// for the program does not cost the report of how it stopped. And it holds
// none of the files the program was started with: a listening socket held
// here would stay bound, after a crash, for as long as the report takes.
func aside(keep uintptr) {
	signal.Ignore(unix.SIGHUP, unix.SIGINT, unix.SIGQUIT, unix.SIGTERM, unix.SIGPIPE)

	for _, descriptor := range inherited() {
		if descriptor != int(keep) { // #nosec G115 -- a descriptor's number
			_ = unix.Close(descriptor)
		}
	}
}

// inherited is the descriptors past the standard ones that this process was
// started with: the open ones that are not close-on-exec, which everything
// the Go runtime and the standard library open is.
func inherited() []int {
	var found []int

	check := func(descriptor int) {
		flags, err := unix.FcntlInt(uintptr(descriptor), unix.F_GETFD, 0) // #nosec G115 -- not negative
		if err == nil && flags&unix.FD_CLOEXEC == 0 {
			found = append(found, descriptor)
		}
	}

	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		// Where the open descriptors cannot be listed, the numbers one may
		// have are tried, up to a bound.
		const most = 1 << 16

		for descriptor := standard; descriptor < most; descriptor++ {
			check(descriptor)
		}

		return found
	}

	for _, entry := range entries {
		if descriptor, err := strconv.Atoi(entry.Name()); err == nil && descriptor >= standard {
			check(descriptor)
		}
	}

	return found
}

// ending reports whether the end of the program beside this watcher is one
// to report: one the runtime had something to say about. How the program
// ended otherwise, a process that is not its parent is not told.
func ending(ended *crash, _ []uintptr) bool {
	return ended.text != ""
}

// given reports whether this process was started with what Watch starts a
// process of that role with, which the variable alone does not give.
//
// A watcher beside a program has a pipe as its first descriptor past the
// standard ones. Its parent is not asked: a program that has crashed
// already is no longer its parent, and a watcher that took itself for the
// program then would run it a second time.
//
// A program under a first process has that process as its parent, and a
// pipe under each of its two numbers.
func given(role string, numbers []uintptr) bool {
	const pair = 2

	if len(numbers) != pair {
		return false
	}

	switch role {
	case roleWatcher:
		return numbers[0] == standard && numbers[1] > 1 && pipe(numbers[0])
	case roleProgram:
		return underFirst() && numbers[0] >= standard && numbers[1] >= standard && numbers[0] != numbers[1] &&
			pipe(numbers[0]) && pipe(numbers[1])
	default:
		return false
	}
}

// pipe reports whether the descriptor of that number is open on a pipe.
func pipe(number uintptr) bool {
	var status unix.Stat_t

	if err := unix.Fstat(int(number), &status); err != nil { // #nosec G115 -- a descriptor's number
		return false
	}

	return status.Mode&unix.S_IFMT == unix.S_IFIFO
}
