//go:build linux

package telemetry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// exitSignalled is added to a signal's number for the exit status of a
	// watcher whose program a signal killed, as a shell does.
	exitSignalled = 128
	// drain is how long the watcher waits for the end of the runtime's text
	// once the program has ended.
	drain = time.Second
)

// first reports whether this process is the first of its container: the one
// whose end is the end of every other.
func first() bool { return os.Getpid() == 1 }

// underFirst reports whether this process is the child of the first one.
func underFirst() bool { return os.Getppid() == 1 }

// above is the watcher as the first process of a container. It starts the
// program as its child, sees it to its end, and returns what the watcher is
// to exit with; or an error when the program could not be started, and then
// the program runs in this process.
//
// It does what the first process of a container is there to do, and nothing
// that changes how the program is run:
//
//   - the program has every file this process was started with, under the
//     same numbers, and the terminal when there is one: what a terminal
//     sends, an interrupt, it sends the program and not the watcher;
//   - every signal sent to this process that a program can be sent is sent
//     on to the program, once;
//   - the processes whose parent ended before them, which the kernel leaves
//     to the first process, are waited for;
//   - a stop asked from the terminal does not hold: nothing can stop the
//     first process of a container, and nothing here would start it again.
func above(config Config) (int, error) {
	// What the runtime prints of a crash, from the program; and the
	// watcher's word, to the program, that it may go on.
	crashes, output, err := os.Pipe()
	if err != nil {
		return 0, fmt.Errorf("telemetry: %w", err)
	}

	defer func() { _ = crashes.Close() }()

	// The end that is read is the program's, and the one that is written
	// the watcher's: closing it is its word.
	heard, word, err := os.Pipe()
	if err != nil {
		_ = output.Close()

		return 0, fmt.Errorf("telemetry: %w", err)
	}

	self, err := os.Executable()
	if err != nil {
		_, _, _ = output.Close(), word.Close(), heard.Close()

		return 0, fmt.Errorf("telemetry: %w", err)
	}

	// A copy of a descriptor is not close-on-exec: the program inherits
	// these two under their own numbers, with everything this process was
	// started with, where a list of files to pass would renumber them all.
	given, err := copies(output, heard)

	_, _ = output.Close(), heard.Close()

	if err != nil {
		_ = word.Close()

		return 0, err
	}

	// Before the program starts, so that no signal falls between the two.
	children := make(chan os.Signal, 1)
	signal.Notify(children, unix.SIGCHLD)

	passed := make(chan os.Signal, 64) //nolint:mnd // room for a burst
	signal.Notify(passed, passable()...)

	// #nosec G702 -- this very program, with its own arguments.
	program, err := os.StartProcess(self, os.Args, &os.ProcAttr{
		Env: append(os.Environ(),
			roleVariable+"="+roleProgram+" "+strconv.Itoa(given[0])+" "+strconv.Itoa(given[1])),
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
		// A group of its own, which gets the terminal below.
		Sys: &syscall.SysProcAttr{Setpgid: true},
	})

	_, _ = unix.Close(given[0]), unix.Close(given[1])

	if err != nil {
		signal.Stop(children)
		signal.Stop(passed)

		_ = word.Close()

		return 0, fmt.Errorf("telemetry: %w", err)
	}

	pid := program.Pid

	// Its status is taken below, with every other child's.
	_ = program.Release()

	// The files the program was started with are the program's alone: a
	// copy held here would keep a socket bound, and whoever reads the
	// program's output waiting, for as long as a report takes after it has
	// ended.
	for _, descriptor := range inherited() {
		_ = unix.Close(descriptor)
	}

	yield(pid)

	// The program goes on from here.
	_ = word.Close()

	said := make(chan crash, 1)

	go func() {
		var printed crash

		printed.text, printed.cut = collected(crashes)
		said <- printed
	}()

	// Once the program has ended, a signal that stops a program stops the
	// watcher: it gives up the report it is at.
	reporting, stop := context.WithCancel(context.Background())
	defer stop()

	tended := &child{pid: pid, stop: stop}

	go tended.pass(passed)

	status := tended.wait(children)

	ended := crash{pid: pid}

	select {
	case ended = <-said:
		ended.pid = pid
	case <-time.After(drain):
	}

	ended.at = time.Now()
	code := status.ExitStatus()

	if status.Signaled() {
		ended.signal = signalName(status.Signal())
		code = exitSignalled + int(status.Signal())
	} else {
		ended.code, ended.exited = code, true
	}

	if ended.text != "" || status.Signaled() && !asked(status.Signal()) {
		report(reporting, config, ended)
	}

	return code, nil
}

// yield gives the program what is its alone, once it has started: the
// terminal, and this process's own hold on its standard input and output.
func yield(pid int) {
	// The terminal is the program's: its group is the one the terminal
	// reads from and signals. This process has it until then, so nothing
	// stops it for giving it away; after, what it writes there must not.
	if group, err := unix.IoctlGetInt(0, unix.TIOCGPGRP); err == nil && group == unix.Getpgrp() {
		_ = unix.IoctlSetPointerInt(0, unix.TIOCSPGRP, pid)
	}

	signal.Ignore(unix.SIGTTOU, unix.SIGPIPE)

	// So are its standard input and output, from here.
	if null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0); err == nil {
		_ = unix.Dup3(int(null.Fd()), 0, 0) // #nosec G115 -- a descriptor's number
		_ = unix.Dup3(int(null.Fd()), 1, 0) // #nosec G115 -- a descriptor's number
		_ = null.Close()
	} else {
		_, _ = os.Stdin.Close(), os.Stdout.Close()
	}
}

// copies is a copy of each file's descriptor, which what this process starts
// next inherits under the copy's number.
func copies(files ...*os.File) ([]int, error) {
	made := make([]int, 0, len(files))

	for _, file := range files {
		descriptor, err := unix.Dup(int(file.Fd())) // #nosec G115 -- a descriptor's number
		if err != nil {
			for _, descriptor = range made {
				_ = unix.Close(descriptor)
			}

			return nil, fmt.Errorf("telemetry: %w", err)
		}

		made = append(made, descriptor)
	}

	return made, nil
}

// child is the program as its first process tends it. A signal is sent to a
// number, and a number whose process has been waited for may be another
// process's the moment after: the two are done under one lock, and no signal
// is sent once the program's status is taken.
type child struct {
	pid int
	// stop is called for a signal that stops a program, when it comes after
	// the program has ended.
	stop func()

	mu     sync.Mutex
	ended  bool
	status unix.WaitStatus
}

// pass sends the program the signals the first process is sent, while there
// is a program.
func (c *child) pass(passed <-chan os.Signal) {
	for sig := range passed {
		number, is := sig.(syscall.Signal)
		if !is {
			continue
		}

		c.mu.Lock()

		switch {
		case !c.ended:
			_ = unix.Kill(c.pid, number)
		case asked(number) || number == unix.SIGQUIT:
			c.stop()
		}

		c.mu.Unlock()
	}
}

// wait takes the status of every child that ends, which the kernel tells of
// on children, until the program has: it returns the program's.
func (c *child) wait(children <-chan os.Signal) unix.WaitStatus {
	for range children {
		if c.reap() {
			break
		}
	}

	return c.status
}

// reap takes the status of every child that has one to give now, and
// reports whether the program has ended.
func (c *child) reap() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	for !c.ended {
		var status unix.WaitStatus

		pid, err := unix.Wait4(-1, &status, unix.WNOHANG|unix.WUNTRACED, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}

		if err != nil || pid <= 0 {
			break
		}

		switch {
		case pid != c.pid:
			// A process left to this one: waited for, and that is all.
		case status.Stopped():
			if stop := status.StopSignal(); stop == unix.SIGTSTP || stop == unix.SIGTTIN || stop == unix.SIGTTOU {
				_ = unix.Kill(c.pid, unix.SIGCONT)
			}
		default:
			c.ended, c.status = true, status
		}
	}

	return c.ended
}

// passable is the signals a first process sends on to its program: all of
// them but the ones no process sends another on purpose, a fault's, the
// kernel's word about a child or a terminal, and the Go runtime's own.
func passable() []os.Signal {
	kept := map[syscall.Signal]bool{
		unix.SIGILL: true, unix.SIGTRAP: true, unix.SIGBUS: true, unix.SIGFPE: true, unix.SIGSEGV: true,
		unix.SIGSTKFLT: true, unix.SIGSYS: true,
		// Not to be caught.
		unix.SIGKILL: true, unix.SIGSTOP: true,
		// The kernel's, about a child, a pipe and a terminal.
		unix.SIGCHLD: true, unix.SIGPIPE: true, unix.SIGTSTP: true, unix.SIGTTIN: true, unix.SIGTTOU: true,
		// The Go runtime's and the C library's.
		unix.SIGURG: true, unix.SIGPROF: true, 32: true, 33: true,
	}

	const last = 64

	var passed []os.Signal

	for number := syscall.Signal(1); number <= last; number++ {
		if !kept[number] {
			passed = append(passed, number)
		}
	}

	return passed
}

// asked reports whether sig is one a program is ended by on purpose: someone
// stopping it, or the reader of its output going away.
func asked(sig syscall.Signal) bool {
	switch sig { //nolint:exhaustive // the four, of all signals
	case unix.SIGINT, unix.SIGTERM, unix.SIGHUP, unix.SIGPIPE:
		return true
	default:
		return false
	}
}

// signalName is a signal as it is written, SIGKILL, where the standard
// library has what it means, "killed".
func signalName(sig syscall.Signal) string {
	if name := unix.SignalName(sig); name != "" {
		return name
	}

	return "signal " + strconv.Itoa(int(sig))
}
