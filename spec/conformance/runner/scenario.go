package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	yaml "go.yaml.in/yaml/v3"
)

// Scenario is one file of spec/conformance/scenarios/, one C-number.
type Scenario struct {
	C          string `yaml:"c"`
	Title      string `yaml:"title"`
	Implements string `yaml:"implements"`
	// Skip, when set, is the reason this scenario cannot be run. It is printed
	// and counted as a SKIP; it never counts as a pass.
	Skip  string `yaml:"skip"`
	Notes string `yaml:"notes"`
	Arms  []Arm  `yaml:"arms"`

	Path string `yaml:"-"`
}

// Arm is one run: one mockd recording, one state directory, one environment.
type Arm struct {
	Name string            `yaml:"name"`
	Env  map[string]string `yaml:"env"`

	// Mock is a mode string (spec/sdk-conformance.md §2 plus mockd's
	// extensions). MockScript is the control socket's per-request program;
	// MockDefault is what governs once the script runs out.
	Mock        string       `yaml:"mock"`
	MockScript  []ScriptStep `yaml:"mock_script"`
	MockDefault string       `yaml:"mock_default"`

	// MockClockFromNow pins mockd's clock to this arm's JELTO_NOW, so that a
	// `stop:<seconds>` computes an `until` in the host's frame rather than in
	// mockd's (mockd doc.go, "clock").
	MockClockFromNow bool `yaml:"mock_clock_from_now"`

	// KeepState reuses the previous arm's state directory, which is how C2's
	// "new process", C4c and C8's persistence rows are written.
	KeepState bool `yaml:"keep_state"`

	Steps  []Step  `yaml:"steps"`
	Assert []Check `yaml:"assert"`
}

type ScriptStep struct {
	Mode  string `yaml:"mode"`
	Times int    `yaml:"times"`
}

// Step is one action. Exactly one field is set.
type Step struct {
	// Run sends host commands, in order, waiting for each reply.
	Run []string `yaml:"run"`
	// Repeat runs the whole `run` list this many times. C1's "track x x1000"
	// and C6's "x1500" are otherwise 1 500 lines of YAML.
	Repeat int `yaml:"repeat"`
	// Await blocks on mockd until `count` requests have arrived in this arm.
	Await *Await `yaml:"await"`
	// WaitMS is a real-time wait in the RUNNER, for the rows that assert
	// something did NOT happen.
	WaitMS int `yaml:"wait_ms"`
	// Mock reconfigures mockd mid-arm (C9's "mockd ok", C16's).
	Mock        string       `yaml:"mock"`
	MockScript  []ScriptStep `yaml:"mock_script"`
	MockDefault string       `yaml:"mock_default"`
	// Restart ends the host process and starts a new one on the same state
	// directory: §4's "new process".
	Restart *Restart `yaml:"restart"`
	// StopHost ends the host process without starting another.
	StopHost bool `yaml:"stop_host"`
	// KillHost ends the host process WITHOUT an `exit` -- SIGKILL, no
	// termination flush, no chance to write anything on the way out. It is
	// what C4c's "the process exits before the delay elapses" actually says,
	// and until 2026-09-02 no scenario could express it: `Host.Kill` existed,
	// its comment claimed to be C4c, and nothing called it (TODO.md §7).
	KillHost bool `yaml:"kill_host"`
}

type Await struct {
	Count     int `yaml:"count"`
	TimeoutMS int `yaml:"timeout_ms"`
	// Optional: fail the arm if the count is reached EARLIER than this many
	// milliseconds after the arm started. Not used by any scenario today; the
	// `no_request_before_s` check is the readable form.
}

type Restart struct {
	Env map[string]string `yaml:"env"`
}

// Check is one assertion. `check` names it; the rest of the fields are its
// parameters, and checks.go says which check reads which.
type Check struct {
	Check string `yaml:"check"`

	Equal       *int           `yaml:"equal"`
	Min         *int           `yaml:"min"`
	Max         *int           `yaml:"max"`
	Value       string         `yaml:"value"`
	Values      []string       `yaml:"values"`
	Counts      map[string]int `yaml:"counts"`
	Request     *int           `yaml:"request"`
	Step        *int           `yaml:"step"`
	Event       *int           `yaml:"event"`
	Field       string         `yaml:"field"`
	Key         string         `yaml:"key"`
	Absent      bool           `yaml:"absent"`
	Contains    []string       `yaml:"contains"`
	NotContains []string       `yaml:"not_contains"`
	Regex       string         `yaml:"regex"`
	Empty       bool           `yaml:"empty"`
	Path        string         `yaml:"path"`

	// schedule / no_request_before_s
	ExpectS      []float64 `yaml:"expect_s"`
	TolerancePct float64   `yaml:"tolerance_pct"`
	SlackMS      int       `yaml:"slack_ms"`
	Seconds      float64   `yaml:"seconds"`
	Anchor       string    `yaml:"anchor"`

	Why string `yaml:"why"`
}

// LoadScenarios reads every *.yaml in dir, sorted by C-number so a run is
// reproducible.
func LoadScenarios(dir string) ([]Scenario, error) {
	entries, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	sort.Strings(entries)
	var scenarios []Scenario
	for _, path := range entries {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		var scenario Scenario
		decoder := yaml.NewDecoder(newReader(raw))
		decoder.KnownFields(true)
		if err := decoder.Decode(&scenario); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		scenario.Path = path
		if scenario.C == "" {
			return nil, fmt.Errorf("%s: no `c:` -- every scenario names the §4 row it is", path)
		}
		scenarios = append(scenarios, scenario)
	}
	sort.SliceStable(scenarios, func(i, j int) bool { return order(scenarios[i].C) < order(scenarios[j].C) })
	return scenarios, nil
}

// order sorts C1 before C10 and puts the W rows last, which is the order §4
// prints them in.
func order(c string) string {
	if len(c) == 0 {
		return c
	}
	prefix, digits, suffix := c[:1], "", ""
	i := 1
	for i < len(c) && c[i] >= '0' && c[i] <= '9' {
		digits += string(c[i])
		i++
	}
	suffix = c[i:]
	return fmt.Sprintf("%s%03s%s", prefix, digits, suffix)
}
