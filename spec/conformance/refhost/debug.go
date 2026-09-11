package main

import (
	"fmt"
	"io"
	"sync"
)

// Debug is RFC-0001 §8.7 item 17: JELTO_DEBUG=1 prints every payload before it
// is sent. When it is off, NOTHING is written -- C10 asserts "nothing written
// to stderr except with JELTO_DEBUG=1", which makes silence part of the
// contract and not merely a preference.
type Debug struct {
	mu      sync.Mutex
	out     io.Writer
	enabled bool
}

func NewDebug(out io.Writer, enabled bool) *Debug {
	return &Debug{out: out, enabled: enabled}
}

func (d *Debug) Enabled() bool { return d.enabled }

func (d *Debug) Printf(format string, args ...any) {
	if !d.enabled {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_, _ = fmt.Fprintf(d.out, "jelto: "+format+"\n", args...)
}

// Payload prints the request body EXACTLY as it will be sent. C17 asserts
// "stderr contains the exact JSON body", so the bytes go out unaltered and on
// their own line -- no pretty-printing, no truncation.
func (d *Debug) Payload(body []byte) {
	if !d.enabled {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_, _ = fmt.Fprint(d.out, "jelto: POST ")
	_, _ = d.out.Write(body)
	_, _ = fmt.Fprintln(d.out)
}
