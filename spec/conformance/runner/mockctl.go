package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// Mock is a client for mockd's control socket. The framing is mockd's doc.go,
// "Control socket": JSON values in, one JSON object per line out, one response
// per command, in order.
type Mock struct {
	conn    net.Conn
	reader  *bufio.Reader
	encoder *json.Encoder
	addr    string
}

type mockReply struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`

	PID     int    `json:"pid"`
	Addr    string `json:"addr"`
	Control string `json:"control"`

	Mode   string `json:"mode"`
	Steps  int    `json:"steps"`
	UnixMS int64  `json:"unix_ms"`

	Next    int64             `json:"next"`
	Records []json.RawMessage `json:"records"`

	Requests        int64 `json:"requests"`
	Connections     int64 `json:"connections"`
	MockErrors      int64 `json:"mock_errors"`
	Dropped         int64 `json:"dropped"`
	ScriptRemaining int   `json:"script_remaining"`
	Listening       bool  `json:"listening"`
}

func DialMock(path string) (*Mock, error) {
	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, err := net.Dial("unix", path)
		if err == nil {
			mock := &Mock{conn: conn, reader: bufio.NewReader(conn), encoder: json.NewEncoder(conn)}
			reply, err := mock.Do(map[string]any{"cmd": "ping"})
			if err != nil {
				return nil, err
			}
			mock.addr = reply.Addr
			return mock, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("dial mockd control socket %s: %w", path, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (m *Mock) Addr() string { return m.addr }

func (m *Mock) Close() { _ = m.conn.Close() }

// Do sends one command and returns its reply. An `ok:false` is an error: a
// scenario that could not be set up must fail at setup, not at assert time
// where it would read as a host bug.
func (m *Mock) Do(command map[string]any) (mockReply, error) {
	// A generous deadline: `await` blocks for as long as the scenario asked.
	_ = m.conn.SetDeadline(time.Now().Add(20 * time.Minute))
	if err := m.encoder.Encode(command); err != nil {
		return mockReply{}, fmt.Errorf("mockd %v: %w", command["cmd"], err)
	}
	line, err := m.reader.ReadBytes('\n')
	if err != nil {
		return mockReply{}, fmt.Errorf("mockd %v: %w", command["cmd"], err)
	}
	var reply mockReply
	if err := json.Unmarshal(line, &reply); err != nil {
		return mockReply{}, fmt.Errorf("mockd %v: %w", command["cmd"], err)
	}
	return reply, nil
}

func (m *Mock) MustDo(command map[string]any) (mockReply, error) {
	reply, err := m.Do(command)
	if err != nil {
		return reply, err
	}
	if !reply.OK {
		return reply, fmt.Errorf("mockd %v: %s", command["cmd"], reply.Error)
	}
	return reply, nil
}

func (m *Mock) Reset() error {
	_, err := m.MustDo(map[string]any{"cmd": "reset"})
	return err
}

func (m *Mock) SetMode(mode string) error {
	_, err := m.MustDo(map[string]any{"cmd": "mode", "mode": mode})
	return err
}

func (m *Mock) SetScript(steps []ScriptStep, fallback string) error {
	command := map[string]any{"cmd": "script", "steps": steps}
	if fallback != "" {
		command["default"] = fallback
	}
	_, err := m.MustDo(command)
	return err
}

func (m *Mock) SetClock(unixMS int64) error {
	_, err := m.MustDo(map[string]any{"cmd": "clock", "unix_ms": unixMS})
	return err
}

// Await blocks until `count` requests exist. A timeout is NOT an error here:
// several §4 rows assert that a request did not arrive, and the assertion
// belongs in the check catalogue, not in the transport.
func (m *Mock) Await(count, timeoutMS int) error {
	reply, err := m.Do(map[string]any{"cmd": "await", "count": count, "since": 0, "timeout_ms": timeoutMS, "limit": 1})
	if err != nil {
		return err
	}
	if !reply.OK && reply.Error != "timeout" {
		return fmt.Errorf("mockd await: %s", reply.Error)
	}
	return nil
}

func (m *Mock) Recording() ([]Record, error) {
	reply, err := m.MustDo(map[string]any{"cmd": "recording", "since": 0, "limit": 0})
	if err != nil {
		return nil, err
	}
	records := make([]Record, 0, len(reply.Records))
	for _, raw := range reply.Records {
		var record Record
		if err := json.Unmarshal(raw, &record); err != nil {
			return nil, fmt.Errorf("decode record: %w", err)
		}
		records = append(records, record)
	}
	return records, nil
}

func (m *Mock) Stats() (mockReply, error) {
	return m.MustDo(map[string]any{"cmd": "stats"})
}

func (m *Mock) Shutdown() { _, _ = m.Do(map[string]any{"cmd": "shutdown"}) }
