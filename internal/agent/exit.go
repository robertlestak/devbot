package agent

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Exit is how a harness's CLI process ended, whatever the CLI printed: what
// the agent knows that the harness container can't see after the fact.
type Exit struct {
	// ExitCode is the shell's: 128 plus the signal's number when a signal
	// ended the process (137 for SIGKILL), as Kubernetes reports a
	// container's.
	ExitCode  int     `json:"exit_code"`
	Signal    string  `json:"signal,omitempty"` // SIGKILL, say; empty when it exited
	DurationS float64 `json:"duration_s"`
	// The last few KiB the CLI wrote to each.
	StderrTail string `json:"stderr_tail,omitempty"`
	StdoutTail string `json:"stdout_tail,omitempty"`
	// OOMSuspected: SIGKILL, and the container's cgroup counted an OOM
	// kill during the run.
	OOMSuspected bool `json:"oom_suspected"`
	// MemoryLimit is the container's memory limit in bytes, when it has
	// one and OOMSuspected.
	MemoryLimit int64 `json:"memory_limit,omitempty"`
}

// Took is the run's duration.
func (e Exit) Took() time.Duration { return time.Duration(e.DurationS * float64(time.Second)) }

// Failed reports whether the process exited non-zero or was killed.
func (e Exit) Failed() bool { return e.ExitCode != 0 || e.Signal != "" }

// LogAttrs are e's fields for slog, the tails included.
func (e Exit) LogAttrs() []any {
	return []any{"exit_code", e.ExitCode, "signal", e.Signal, "duration_s", e.DurationS, "oom_suspected", e.OOMSuspected,
		"memory_limit", e.MemoryLimit, "stderr_tail", e.StderrTail, "stdout_tail", e.StdoutTail}
}

// exitTail is how much of each stream Exit keeps.
const exitTail = 4 << 10

// The container's cgroup v2 files: the OOM killer's count, and the limit.
var (
	MemoryEventsFile = "/sys/fs/cgroup/memory.events"
	MemoryMaxFile    = "/sys/fs/cgroup/memory.max"
)

// RunCLI runs a harness's CLI with RunGroup, its output into stdout and
// stderr, and reports how it ended. err is RunGroup's: an *exec.ExitError
// when the CLI exited non-zero or was killed, something else when it never
// ran.
func RunCLI(cmd *exec.Cmd, stdout, stderr *bytes.Buffer) (Exit, error) {
	cmd.Stdout, cmd.Stderr = stdout, stderr
	ooms := oomKills(MemoryEventsFile)
	start := time.Now()
	err := RunGroup(cmd)
	e := Exit{DurationS: time.Since(start).Seconds(), StderrTail: Tail(stderr.String(), exitTail), StdoutTail: Tail(stdout.String(), exitTail)}
	if st := cmd.ProcessState; st != nil {
		e.ExitCode = st.ExitCode()
		if ws, ok := st.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			e.ExitCode, e.Signal = 128+int(ws.Signal()), SignalName(ws.Signal())
		}
	}
	// The kernel's OOM killer sends SIGKILL; the count tells it apart from
	// a timeout's kill or a person's.
	if e.Signal == "SIGKILL" && oomKills(MemoryEventsFile) > ooms {
		e.OOMSuspected = true
		e.MemoryLimit = memoryLimit(MemoryMaxFile)
	}
	return e, err
}

// oomKills reads oom_kill from a cgroup v2 memory.events file: 0 when
// there is none, as outside a container.
func oomKills(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	for l := range strings.Lines(string(b)) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "oom_kill "); ok {
			n, _ := strconv.Atoi(strings.TrimSpace(v))
			return n
		}
	}
	return 0
}

// memoryLimit reads a cgroup v2 memory.max file: 0 for "max" or none.
func memoryLimit(path string) int64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return n
}

var signalNames = map[syscall.Signal]string{
	syscall.SIGKILL: "SIGKILL", syscall.SIGTERM: "SIGTERM", syscall.SIGINT: "SIGINT", syscall.SIGHUP: "SIGHUP",
	syscall.SIGQUIT: "SIGQUIT", syscall.SIGABRT: "SIGABRT", syscall.SIGSEGV: "SIGSEGV", syscall.SIGBUS: "SIGBUS",
	syscall.SIGPIPE: "SIGPIPE", syscall.SIGXCPU: "SIGXCPU", syscall.SIGILL: "SIGILL", syscall.SIGFPE: "SIGFPE",
}

// SignalName is a signal's SIG name, for the ones a run dies of.
func SignalName(s syscall.Signal) string {
	if n, ok := signalNames[s]; ok {
		return n
	}
	return fmt.Sprintf("signal %d", int(s))
}

// Bytes formats a memory size as Kubernetes writes one: 8Gi, 512Mi.
func Bytes(n int64) string {
	for _, u := range []struct {
		suffix string
		size   int64
	}{{"Gi", 1 << 30}, {"Mi", 1 << 20}, {"Ki", 1 << 10}} {
		if n >= u.size && n%u.size == 0 {
			return strconv.FormatInt(n/u.size, 10) + u.suffix
		}
	}
	if n >= 1<<30 {
		return fmt.Sprintf("%.1fGi", float64(n)/(1<<30))
	}
	return strconv.FormatInt(n, 10)
}
