//go:build windows

package telemetry

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sys/windows"
)

const (
	// queryLimited is PROCESS_QUERY_LIMITED_INFORMATION: the right to read a
	// process's exit status, and little else.
	queryLimited = 0x1000
	// faults is where the statuses of a fault begin: an NTSTATUS of the
	// severity of an error, which is what a process Windows ended for an
	// exception exits with.
	faults = 0xC0000000
	// interrupted is STATUS_CONTROL_C_EXIT, the one of them that is someone
	// stopping the program: what Ctrl-C ends a process with when it does not
	// handle it.
	interrupted = 0xC000013A
)

// begin starts the watcher of this program, beside it. It is handed the end
// it reads of the pipe the runtime writes a crash to, and the program's own
// process, to read what it exited with.
func begin(Config) error {
	reader, writer, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("telemetry: %w", err)
	}

	defer func() { _ = reader.Close() }()

	failed := func(err error) error {
		_ = writer.Close()

		return fmt.Errorf("telemetry: %w", err)
	}

	self, err := syscall.GetCurrentProcess()
	if err != nil {
		return failed(err)
	}

	// Handles the watcher inherits: copies, since no handle the standard
	// library opens is inherited.
	var output, program syscall.Handle

	err = syscall.DuplicateHandle(
		self,
		syscall.Handle(reader.Fd()),
		self,
		&output,
		0,
		true,
		syscall.DUPLICATE_SAME_ACCESS,
	)
	if err != nil {
		return failed(err)
	}

	defer func() { _ = syscall.CloseHandle(output) }()

	if err = syscall.DuplicateHandle(
		self,
		self,
		self,
		&program,
		syscall.SYNCHRONIZE|queryLimited,
		true,
		0,
	); err != nil {
		return failed(err)
	}

	defer func() { _ = syscall.CloseHandle(program) }()

	second, err := again(roleWatcher, uintptr(output), uintptr(os.Getpid()), uintptr(program))
	if err != nil {
		_ = writer.Close()

		return err
	}

	second.Stderr = os.Stderr
	second.SysProcAttr = &syscall.SysProcAttr{
		// A group of its own: a Ctrl-C at the console is the program's.
		CreationFlags:              syscall.CREATE_NEW_PROCESS_GROUP,
		AdditionalInheritedHandles: []syscall.Handle{output, program},
	}

	if err = second.Start(); err != nil {
		return failed(err)
	}

	_ = second.Process.Release()

	return crashOutput(writer)
}

// aside sets the watcher beside a program apart from it: it ends when the
// program does, and at no one's word before.
func aside(uintptr) {
	signal.Ignore(os.Interrupt)

	// A console that closes, a session that ends: Windows ends the process
	// soon after, whatever it does. Listening keeps the Go runtime from
	// ending it sooner.
	signal.Notify(make(chan os.Signal, 1), syscall.SIGTERM)
}

// ending reads what the program beside this watcher exited with, and reports
// whether its end is one to report: one the runtime had something to say
// about, or an exit with the status of a fault.
func ending(ended *crash, numbers []uintptr) bool {
	const withProgram = 3

	if len(numbers) < withProgram {
		return ended.text != ""
	}

	program := syscall.Handle(numbers[2])

	defer func() { _ = syscall.CloseHandle(program) }()

	// The pipe has ended, so the program has, or is about to.
	if event, err := syscall.WaitForSingleObject(
		program,
		syscall.INFINITE,
	); err != nil ||
		event != syscall.WAIT_OBJECT_0 {
		return ended.text != ""
	}

	var status uint32

	if err := syscall.GetExitCodeProcess(program, &status); err != nil {
		return ended.text != ""
	}

	ended.code, ended.exited = int(status), true

	return ended.text != "" || status >= faults && status != interrupted
}

// given reports whether this process was started with what Watch starts a
// watcher with: the program as its parent, a pipe under the first number,
// and under the third a handle of the program's own process. No program is
// under a first process here.
func given(role string, numbers []uintptr) bool {
	const withProgram = 3

	if role != roleWatcher || len(numbers) != withProgram {
		return false
	}

	if kind, err := windows.GetFileType(windows.Handle(numbers[0])); err != nil || kind != windows.FILE_TYPE_PIPE {
		return false
	}

	program, err := windows.GetProcessId(windows.Handle(numbers[2]))

	return err == nil && uintptr(program) == numbers[1] &&
		uintptr(os.Getppid()) == numbers[1] // #nosec G115 -- a process's number
}
