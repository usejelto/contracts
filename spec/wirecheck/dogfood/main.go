// Command dogfood validates captured SDK debug payloads and reports client-side
// loss and exercised wire fields. Server counters cannot expose values dropped
// before sending, and the enriched archive does not retain original wire bodies.
//
// Schema failures or client-side loss fail the run; server rejection echoes are
// reported for comparison with server counters.
//
//	go run ./spec/wirecheck/dogfood -capture eqbase-week.log
//	make dogfood-wire CAPTURE=eqbase-week.log
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"jelto.io/jelto/contracts/spec/wirecheck"
)

// Read well beyond the 65,536-byte wire cap so oversized bodies are findings, not scanner truncation errors.
const maxCaptureLine = 1 << 20

func main() {
	capture := flag.String("capture", "-", "captured SDK debug log; `-` reads stdin")
	schemaPath := flag.String("schema", "", "path to spec/wire-v1.schema.json (default: beside this source tree)")
	verbose := flag.Bool("v", false, "print every classified non-body line, not only the losses")
	flag.Parse()

	if *schemaPath == "" {
		root, err := repoRoot()
		if err != nil {
			fatal("locate the repository root: %v", err)
		}
		*schemaPath = filepath.Join(root, "spec", "wire-v1.schema.json")
	}

	validator, err := wirecheck.New(*schemaPath)
	if err != nil {
		fatal("%v", err)
	}

	input := io.Reader(os.Stdin)
	name := "(stdin)"
	if *capture != "-" {
		file, err := os.Open(*capture)
		if err != nil {
			fatal("open capture: %v", err)
		}
		defer file.Close()
		input = file
		name = *capture
	}

	report, err := run(input, validator, *verbose)
	if err != nil {
		fatal("read capture: %v", err)
	}
	report.print(name, validator.Path())
	if report.failed() {
		os.Exit(1)
	}
}

type report struct {
	lines        int
	skipped      int
	census       *wirecheck.Census
	invalid      []string
	oversize     []string
	losses       []wirecheck.Line
	rejections   []wirecheck.Line
	batchDrops   []wirecheck.Line
	flushGaveUp  []wirecheck.Line
	operational  int
	verboseLines []wirecheck.Line
}

func run(input io.Reader, validator *wirecheck.Validator, verbose bool) (*report, error) {
	r := &report{census: wirecheck.NewCensus()}
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 0, 64*1024), maxCaptureLine)

	for number := 1; scanner.Scan(); number++ {
		r.lines = number
		line := wirecheck.Classify(number, scanner.Text())
		switch line.Kind {
		case wirecheck.KindOther:
			r.skipped++
		case wirecheck.KindPost:
			body := []byte(line.Message)
			// W1's transport half, which the schema cannot express.
			if len(body) > wirecheck.MaxBodyBytes {
				r.oversize = append(r.oversize, fmt.Sprintf(
					"line %d: body is %d bytes, spec/wire-v1.md §2 caps it at %d",
					number, len(body), wirecheck.MaxBodyBytes))
			}
			if err := r.census.Add(body); err != nil {
				r.invalid = append(r.invalid, fmt.Sprintf("line %d: %v", number, err))
				continue
			}
			if err := validator.ValidateBody(body); err != nil {
				r.invalid = append(r.invalid, fmt.Sprintf("line %d does not validate:\n    %v",
					number, indent(err.Error())))
			}
		case wirecheck.KindClientLoss:
			r.losses = append(r.losses, line)
		case wirecheck.KindServerRejection:
			r.rejections = append(r.rejections, line)
		case wirecheck.KindBatchDropped:
			r.batchDrops = append(r.batchDrops, line)
		case wirecheck.KindFlushGaveUp:
			r.flushGaveUp = append(r.flushGaveUp, line)
		case wirecheck.KindOperational:
			r.operational++
			if verbose {
				r.verboseLines = append(r.verboseLines, line)
			}
		}
	}
	return r, scanner.Err()
}

// Only schema failures and client-side loss fail the verdict; other findings are reported.
func (r *report) failed() bool {
	return len(r.invalid) > 0 || len(r.oversize) > 0 || len(r.losses) > 0
}

func (r *report) print(capture, schema string) {
	fmt.Printf("spec/wirecheck -- %s against a captured SDK debug log\n", filepath.Base(schema))
	fmt.Printf("  capture   %s (%d lines, %d not written by the SDK)\n", capture, r.lines, r.skipped)
	fmt.Printf("  schema    %s\n\n", schema)

	c := r.census
	fmt.Printf("BODIES\n")
	fmt.Printf("  %d POST %s, %d events, %d rejected by the schema\n",
		c.Bodies, plural(c.Bodies, "body", "bodies"), c.Events, len(r.invalid))
	fmt.Printf("  largest body %d B of %d (W1) · most events in one body %d of %d (W1)\n\n",
		c.LargestBody, wirecheck.MaxBodyBytes, c.MostEvents, wirecheck.MaxEvents)
	for _, failure := range r.invalid {
		fmt.Printf("  FAIL %s\n", failure)
	}
	for _, failure := range r.oversize {
		fmt.Printf("  FAIL %s\n", failure)
	}
	if len(r.invalid)+len(r.oversize) > 0 {
		fmt.Println()
	}

	fmt.Printf("FIELDS OBSERVED  (after the freeze a grammar is fixed whether or not a sender exercised it)\n")
	for _, surface := range []string{"app", "web", "(no s)"} {
		fields, ok := c.FieldsBySurf[surface]
		if !ok {
			continue
		}
		fmt.Printf("  s=%-7s %s\n", surface, wirecheck.Sorted(fields))
		if unseen := c.Unseen(surface); len(unseen) > 0 {
			fmt.Printf("  %-9s NOT SEEN: %s\n", "", strings.Join(unseen, ", "))
		}
	}
	fmt.Printf("  events    %s\n", wirecheck.Sorted(c.EventNames))
	fmt.Printf("  props     %s\n", wirecheck.Sorted(c.PropKeys))
	fmt.Printf("  v         %s\n", wirecheck.Sorted(c.ClientVers))
	fmt.Printf("  p         %s\n\n", wirecheck.Sorted(c.ProductKeys))

	fmt.Printf("CLIENT-SIDE LOSSES  (item 1's real question -- NONE of these reaches an Ops counter)\n")
	if len(r.losses) == 0 {
		fmt.Printf("  none\n\n")
	} else {
		for _, line := range r.losses {
			fmt.Printf("  FAIL line %d: %s\n         lost: %s\n         from: %s\n",
				line.Number, line.Message, line.Lost, line.Source)
		}
		fmt.Println()
	}

	fmt.Printf("ALSO IN THE CAPTURE  (reported, does not decide item 1)\n")
	fmt.Printf("  %d server-side rejections echoed -- cross-check these against the Ops card\n", len(r.rejections))
	for _, line := range r.rejections {
		fmt.Printf("      line %d: %s\n", line.Number, line.Message)
	}
	fmt.Printf("  %d batch drops (a 400/402 discarded a whole batch; an ENVELOPE 400 is counted\n", len(r.batchDrops))
	fmt.Printf("      nowhere -- wire §6's exemption class, no product to attribute it to)\n")
	for _, line := range r.batchDrops {
		fmt.Printf("      line %d: %s\n", line.Number, line.Message)
	}
	fmt.Printf("  %d terminations that gave up with events still queued (RFC-0001 §8.3 item 7)\n", len(r.flushGaveUp))
	for _, line := range r.flushGaveUp {
		fmt.Printf("      line %d: %s\n", line.Number, line.Message)
	}
	fmt.Printf("  %d operational lines (retry, kill switch, backoff)\n", r.operational)
	for _, line := range r.verboseLines {
		fmt.Printf("      line %d: %s\n", line.Number, line.Message)
	}
	fmt.Println()

	if r.failed() {
		fmt.Printf("ITEM 1: FAIL -- the wire freeze is blocked (spec/wire-v1.md §10)\n")
		return
	}
	if c.Bodies == 0 {
		fmt.Printf("ITEM 1: NOT ANSWERED -- the capture holds no `jelto: POST` line. An empty capture\n")
		fmt.Printf("        is not a pass: check that the build ran with `Jelto.debug = true`.\n")
		return
	}
	fmt.Printf("ITEM 1: PASS over this capture -- %d %s, %d events, no client-side loss\n",
		c.Bodies, plural(c.Bodies, "body", "bodies"), c.Events)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func indent(s string) string { return strings.ReplaceAll(s, "\n", "\n    ") }

func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod above the working directory")
		}
		dir = parent
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "wirecheck: "+format+"\n", args...)
	os.Exit(2)
}
