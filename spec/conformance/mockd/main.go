package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

// Ready is the single line mockd writes to STDOUT once both listeners are up,
// before anything else. A runner that started mockd with `-addr 127.0.0.1:0`
// learns the port from here; a runner that started it on a fixed port uses it
// as the readiness signal, which is what stops a scenario racing the bind.
// Everything else mockd says goes to stderr through slog, so stdout carries
// exactly this one line.
type Ready struct {
	Addr    string `json:"addr"`
	Control string `json:"control"`
	PID     int    `json:"pid"`
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "mockd:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("mockd", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		fmt.Fprint(stderr, usage)
		flags.PrintDefaults()
	}
	var (
		addr       = flags.String("addr", "127.0.0.1:0", "HTTP listen address; port 0 picks one and reports it in the ready line")
		control    = flags.String("control", "", "control socket path (default <tmp>/mockd-<pid>.sock)")
		initial    = flags.String("mode", "ok", "initial default mode (spec/sdk-conformance.md §2)")
		record     = flags.String("record", "", "append every record to this file as JSON lines")
		maxRecords = flags.Int("max-records", 100000, "records kept in memory; the oldest are dropped and counted")
		maxBody    = flags.Int64("max-body", 1<<20, "bytes of each request body kept; the true length is always recorded")
		logLevel   = flags.String("log-level", "info", "debug, info, warn or error")
	)
	if err := flags.Parse(args); err != nil {
		return err
	}

	level, err := parseLevel(*logLevel)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))

	mode, err := ParseMode(*initial)
	if err != nil {
		return fmt.Errorf("-mode: %w", err)
	}

	recording, err := NewRecording(*maxRecords, *record)
	if err != nil {
		return fmt.Errorf("-record: %w", err)
	}
	defer recording.Close()

	server := NewServer(log, recording, mode, *maxBody)
	if err := server.Listen(*addr); err != nil {
		return fmt.Errorf("-addr %s: %w", *addr, err)
	}
	defer server.Close()

	controller, err := NewControl(log, server, *control)
	if err != nil {
		return err
	}
	defer controller.Close()
	go controller.Serve()

	// The initial mode is applied only after both listeners exist, so that
	// `-mode down` starts refused rather than never bound: the address must be
	// resolved before it can be given up and taken back.
	if mode.Down {
		server.BringDown()
	}

	line, err := json.Marshal(Ready{Addr: server.Addr(), Control: controller.Path(), PID: os.Getpid()})
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "%s\n", line); err != nil {
		return err
	}
	if flusher, ok := stdout.(interface{ Sync() error }); ok {
		_ = flusher.Sync()
	}
	log.Info("mockd ready", "addr", server.Addr(), "control", controller.Path(), "mode", mode.Raw)

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)

	select {
	case sig := <-signals:
		log.Info("mockd stopping", "signal", sig.String())
	case <-controller.Shutdown():
		log.Info("mockd stopping", "reason", "control shutdown")
	}
	return nil
}

func parseLevel(name string) (slog.Level, error) {
	switch strings.ToLower(name) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("-log-level %q: want debug, info, warn or error", name)
}

const usage = `mockd -- the mock ingest endpoint of spec/sdk-conformance.md §1.

POST /v1/e with scriptable behaviour (§2) and a recording of every request.
Modes come from the X-Mock header a conformance host forwards from JELTO_MOCK,
or from the control socket; spec/conformance/mockd/doc.go is the full contract
for both, and for what the recording carries.

On start mockd writes one JSON line to stdout:

    {"addr":"127.0.0.1:53712","control":"/tmp/mockd-4711.sock","pid":4711}

Flags:
`
