# This file is the project's own.
# Add recipes leveraging provided `do` ready-made recipes, or create your own.
# The import must be kept: it mounts every shared limen task under `just do ...`.
import '.limen/just/main.just'

# The FIRST recipe defined here becomes `just`'s default.
lint: do::lint::go::default do::lint::go::deadcode do::lint::default
fix: do::fix::go::default do::fix::default
test: do::test::go::unit do::test::go::race do::test::go::cover
security: do::security::default

# The watcher as a container's first process is tested by starting the program
# as number 1 of a new namespace of processes, which the kernel lets root do
# and, on a hosted runner, no one else: in the shared lanes those tests skip,
# and a skip proves nothing. This runs the suite as root and takes a skip for
# a failure.
[doc("Run the suite as root, where the tests of the watcher as a container's first process do not skip (Linux only)")]
test-first-process:
    #!/usr/bin/env bash
    set -euo pipefail
    host="$(go env GOHOSTOS)"
    if [ "$host" != "linux" ]; then
        echo "test-first-process: a namespace of processes is Linux's; this host is $host" >&2
        exit 1
    fi
    out="${LIMEN_BUILD:-.build}/first-process"
    mkdir -p "$out"
    go test -c -o "$out/telemetry.test" .
    sudo -n "$out/telemetry.test" -test.v 2>&1 | tee "$out/log"
    if grep -Eq '^[[:space:]]*--- SKIP' "$out/log"; then
        echo "test-first-process: a test skipped, which here is a test that did not run:" >&2
        grep -E '^[[:space:]]*--- SKIP' "$out/log" >&2
        exit 1
    fi
    grep -q '^--- PASS: TestTheFirstProcessOfAContainerIsTheWatcher' "$out/log" || {
        echo "test-first-process: the suite passed but the first process's tests are not among it" >&2
        exit 1
    }
    echo "test-first-process: the suite passed as root, with no test skipped"
