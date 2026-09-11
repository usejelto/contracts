package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// HostReply marks completion of one command under spec/sdk-conformance.md §3.1.
type HostReply struct {
	Cmd    string `json:"cmd"`
	OK     bool   `json:"ok"`
	Error  string `json:"error"`
	Value  string `json:"value"`
	Micros int64  `json:"us"`

	// State carries dumpstate's semantic export as raw JSON for field-specific decoding.
	State json.RawMessage `json:"state"`

	Tracks       int   `json:"tracks"`
	TrackP50US   int64 `json:"track_p50_us"`
	TrackP99US   int64 `json:"track_p99_us"`
	HeapDeltaKiB int64 `json:"heap_delta_kib"`
}

// Host is one running conformance-host process.
type Host struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader

	mu      sync.Mutex
	stderr  bytes.Buffer
	replies []HostReply

	exited   bool
	exitCode int
	waitErr  error
	waitOnce sync.Once
}

// StartHost launches the host with the environment of §3 plus whatever the arm
// adds.
func StartHost(binary string, env map[string]string) (*Host, error) {
	cmd := exec.Command(binary)
	// Remove ambient JELTO_ variables so developer settings cannot change scenario behavior.
	cmd.Env = append(withoutJelto(os.Environ()), flattenEnv(env)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	host := &Host{cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout)}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		buffer := make([]byte, 4096)
		for {
			n, err := stderr.Read(buffer)
			if n > 0 {
				host.mu.Lock()
				host.stderr.Write(buffer[:n])
				host.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	return host, nil
}

func withoutJelto(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, entry := range environ {
		if strings.HasPrefix(entry, "JELTO_") {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func flattenEnv(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for key, value := range env {
		out = append(out, key+"="+value)
	}
	return out
}

// Send fails an unresponsive host on a real-time deadline.
func (h *Host) Send(command string, timeout time.Duration) (HostReply, error) {
	if _, err := io.WriteString(h.stdin, command+"\n"); err != nil {
		return HostReply{}, fmt.Errorf("write %q to host: %w", command, err)
	}
	type result struct {
		line []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		line, err := h.stdout.ReadBytes('\n')
		done <- result{line, err}
	}()
	select {
	case got := <-done:
		if got.err != nil {
			return HostReply{}, fmt.Errorf("host reply to %q: %w", command, got.err)
		}
		line := bytes.TrimSpace(got.line)
		var reply HostReply
		if err := json.Unmarshal(line, &reply); err != nil {
			return HostReply{}, fmt.Errorf("host reply to %q is not JSON (%q): %w -- spec/sdk-conformance.md §3.1 makes stdout the reply channel and NOTHING else, for the host and for the SDK it links; a line here that is not a reply is something printing to stdout that belongs on stderr", command, line, err)
		}
		// Validate the command echo: stray valid JSON could otherwise shift every later reply by one.
		if word := commandWord(command); reply.Cmd != word {
			return HostReply{}, fmt.Errorf("host answered %q with a reply whose `cmd` is %q, not %q (%q) -- spec/sdk-conformance.md §3.1: one reply per command and `cmd` echoes the command word. A JSON line on stdout that is not this command's reply desynchronises the channel", command, reply.Cmd, word, line)
		}
		h.mu.Lock()
		h.replies = append(h.replies, reply)
		h.mu.Unlock()
		return reply, nil
	case <-time.After(timeout):
		return HostReply{}, fmt.Errorf("host did not answer %q within %s", command, timeout)
	}
}

// Stop sends `exit` and waits. A host that will not exit is killed and the arm
// reports it: C1's "process exits within 1 s" is a real assertion.
func (h *Host) Stop(timeout time.Duration) (exitCode int, err error) {
	if h.exited {
		return h.exitCode, h.waitErr
	}
	_, sendErr := h.Send("exit", timeout)
	_ = h.stdin.Close()
	waited := make(chan error, 1)
	go func() { waited <- h.cmd.Wait() }()
	select {
	case waitErr := <-waited:
		h.finish(waitErr)
	case <-time.After(timeout):
		_ = h.cmd.Process.Kill()
		<-waited
		h.finish(fmt.Errorf("host did not exit within %s of `exit`", timeout))
	}
	if sendErr != nil && h.waitErr == nil {
		h.waitErr = sendErr
	}
	return h.exitCode, h.waitErr
}

// Kill implements abrupt termination without a flush or state write. C4c uses it
// to require persistence before termination, not merely in an exit hook.
func (h *Host) Kill() {
	if h.exited {
		return
	}
	_ = h.stdin.Close()
	_ = h.cmd.Process.Kill()
	_ = h.cmd.Wait()
	h.finish(nil)
}

func (h *Host) finish(waitErr error) {
	h.waitOnce.Do(func() {
		h.exited = true
		h.exitCode = h.cmd.ProcessState.ExitCode()
		if waitErr != nil {
			var exit *exec.ExitError
			if !errorsAs(waitErr, &exit) {
				h.waitErr = waitErr
			}
		}
	})
}

func errorsAs(err error, target **exec.ExitError) bool {
	if exit, ok := err.(*exec.ExitError); ok {
		*target = exit
		return true
	}
	return false
}

func commandWord(command string) string {
	if fields := strings.Fields(command); len(fields) > 0 {
		return fields[0]
	}
	return ""
}

func (h *Host) Stderr() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.stderr.String()
}

func (h *Host) Replies() []HostReply {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]HostReply(nil), h.replies...)
}

func (h *Host) ExitCode() int { return h.exitCode }
