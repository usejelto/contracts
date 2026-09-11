package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Control is the runner's channel into mockd: doc.go, "Control socket", is the
// contract this file implements.
type Control struct {
	log      *slog.Logger
	server   *Server
	path     string
	listener net.Listener
	shutdown chan struct{}
	once     sync.Once
}

// Command is one line in.
type Command struct {
	Cmd string `json:"cmd"`

	Mode    string       `json:"mode,omitempty"`
	Steps   []scriptStep `json:"steps,omitempty"`
	Default *string      `json:"default,omitempty"`

	UnixMS int64 `json:"unix_ms,omitempty"`

	Since     int64 `json:"since,omitempty"`
	Limit     int64 `json:"limit,omitempty"`
	Count     int   `json:"count,omitempty"`
	TimeoutMS int   `json:"timeout_ms,omitempty"`
}

// Reply is one line out. Every field is omitempty except OK, so a reply says
// only what its command produced and a runner never has to tell an absent field
// from a zero one it did not ask for.
type Reply struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`

	PID      int    `json:"pid,omitempty"`
	Addr     string `json:"addr,omitempty"`
	Control  string `json:"control,omitempty"`
	UptimeMS int64  `json:"uptime_ms,omitempty"`

	Mode   string `json:"mode,omitempty"`
	Steps  int    `json:"steps,omitempty"`
	UnixMS int64  `json:"unix_ms,omitempty"`

	Next    int64             `json:"next,omitempty"`
	Records []json.RawMessage `json:"records,omitempty"`

	Requests        *int64 `json:"requests,omitempty"`
	Connections     *int64 `json:"connections,omitempty"`
	MockErrors      *int64 `json:"mock_errors,omitempty"`
	Dropped         *int64 `json:"dropped,omitempty"`
	ScriptRemaining *int   `json:"script_remaining,omitempty"`
	Listening       *bool  `json:"listening,omitempty"`
}

// DefaultControlPath is the socket a runner finds when -control is not given.
// It carries the pid so two mockds on one machine cannot collide.
func DefaultControlPath() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("mockd-%d.sock", os.Getpid()))
}

func NewControl(log *slog.Logger, server *Server, path string) (*Control, error) {
	if path == "" {
		path = DefaultControlPath()
	}
	// A stale socket from a killed run would otherwise make every start fail
	// with "address already in use" on a file nothing is listening to.
	if info, err := os.Stat(path); err == nil && info.Mode()&os.ModeSocket != 0 {
		_ = os.Remove(path)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("control socket %s: %w", path, err)
	}
	// The socket both changes the endpoint's behaviour and hands back every
	// recorded body, so it is owner-only.
	_ = os.Chmod(path, 0o600)
	return &Control{log: log, server: server, path: path, listener: listener, shutdown: make(chan struct{})}, nil
}

func (c *Control) Path() string { return c.path }

// Shutdown is closed when a `shutdown` command arrives.
func (c *Control) Shutdown() <-chan struct{} { return c.shutdown }

func (c *Control) Serve() {
	for {
		conn, err := c.listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			c.log.Error("control accept failed", "error", err)
			return
		}
		go c.session(conn)
	}
}

func (c *Control) Close() {
	_ = c.listener.Close()
	_ = os.Remove(c.path)
}

// session serves one control connection: JSON values in, exactly one reply
// object per command out, in order. A command that cannot be read is answered
// and the connection stays open -- a runner with a typo in one scenario should
// see the error, not lose the socket.
func (c *Control) session(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	decoder := json.NewDecoder(conn)
	encoder := json.NewEncoder(conn)
	for {
		var command Command
		if err := decoder.Decode(&command); err != nil {
			if !errors.Is(err, io.EOF) {
				_ = encoder.Encode(Reply{Error: "cannot read command: " + err.Error()})
			}
			return
		}
		reply, stop := c.dispatch(command)
		if err := encoder.Encode(reply); err != nil {
			return
		}
		if stop {
			c.signalShutdown()
			return
		}
	}
}

// signalShutdown is idempotent: two control connections may send `shutdown` at
// once, and closing the channel twice would panic.
func (c *Control) signalShutdown() {
	c.once.Do(func() { close(c.shutdown) })
}

func (c *Control) dispatch(command Command) (Reply, bool) {
	switch command.Cmd {
	case "ping":
		return Reply{
			OK:       true,
			PID:      os.Getpid(),
			Addr:     c.server.Addr(),
			Control:  c.path,
			UptimeMS: time.Since(c.server.started).Milliseconds(),
		}, false

	case "mode":
		mode, err := ParseMode(command.Mode)
		if err != nil {
			// Parsed HERE so a bad scenario fails at setup rather than at
			// assert time, where it would read as an SDK bug.
			return Reply{Error: err.Error()}, false
		}
		if err := c.server.SetMode(mode); err != nil {
			return Reply{Error: err.Error()}, false
		}
		c.log.Info("mockd mode set", "mode", mode.Raw)
		return Reply{OK: true, Mode: mode.Raw}, false

	case "script":
		steps, err := expandScript(command.Steps)
		if err != nil {
			return Reply{Error: err.Error()}, false
		}
		var fallback *Mode
		if command.Default != nil {
			mode, err := ParseMode(*command.Default)
			if err != nil {
				return Reply{Error: "default: " + err.Error()}, false
			}
			if mode.Down {
				return Reply{Error: "a script default of `down` would take the listener away at an unpredictable step; set it with {\"cmd\":\"mode\"}"}, false
			}
			fallback = &mode
		}
		c.server.SetScript(steps, fallback)
		if err := c.server.BringUp(); err != nil {
			return Reply{Error: err.Error()}, false
		}
		c.log.Info("mockd script set", "steps", len(steps))
		return Reply{OK: true, Steps: len(steps)}, false

	case "clock":
		c.server.SetClock(command.UnixMS)
		return Reply{OK: true, UnixMS: command.UnixMS}, false

	case "recording":
		records, next, _ := c.server.recording.Since(command.Since, command.Limit)
		dropped := c.server.recording.Stats().Dropped
		return Reply{OK: true, Next: next, Records: records, Dropped: &dropped}, false

	case "await":
		timeout := time.Duration(command.TimeoutMS) * time.Millisecond
		if timeout <= 0 {
			timeout = 10 * time.Second
		}
		reached := c.server.Await(command.Since, command.Count, timeout)
		records, next, _ := c.server.recording.Since(command.Since, command.Limit)
		if !reached {
			return Reply{Error: "timeout", Next: next, Records: records}, false
		}
		return Reply{OK: true, Next: next, Records: records}, false

	case "reset":
		if err := c.server.Reset(); err != nil {
			return Reply{Error: err.Error()}, false
		}
		return Reply{OK: true, Addr: c.server.Addr()}, false

	case "stats":
		stats := c.server.recording.Stats()
		remaining := c.server.ScriptRemaining()
		listening := c.server.Listening()
		return Reply{
			OK:              true,
			Mode:            c.server.DefaultMode().Raw,
			Requests:        &stats.Requests,
			Connections:     &stats.Connections,
			MockErrors:      &stats.MockErrors,
			Dropped:         &stats.Dropped,
			ScriptRemaining: &remaining,
			Listening:       &listening,
		}, false

	case "shutdown":
		return Reply{OK: true}, true

	case "":
		return Reply{Error: "no `cmd`"}, false
	}
	return Reply{Error: fmt.Sprintf("unknown command %q; see spec/conformance/mockd/doc.go", command.Cmd)}, false
}

// expandScript flattens `times` at set time, so consuming a step at request
// time is a single slice index and cannot be got wrong under load.
func expandScript(steps []scriptStep) ([]Mode, error) {
	var out []Mode
	for i, step := range steps {
		mode, err := ParseMode(step.Mode)
		if err != nil {
			return nil, fmt.Errorf("step %d (%q): %w", i, step.Mode, err)
		}
		if mode.Down {
			return nil, fmt.Errorf("step %d: `down` is not a per-request mode; there is no request to answer when the listener is closed", i)
		}
		times := step.Times
		if times == 0 {
			times = 1
		}
		if times < 0 || times > 100000 {
			return nil, fmt.Errorf("step %d: `times` must be between 1 and 100000", i)
		}
		for n := 0; n < times; n++ {
			out = append(out, mode)
		}
	}
	return out, nil
}
