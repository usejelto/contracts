package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"jelto.io/jelto/contracts/spec/wirecheck"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "runner:", err)
		os.Exit(1)
	}
}

func validateContractsVersion(root, required string) error {
	raw, err := os.ReadFile(filepath.Join(root, "spec", "contracts", "manifest.json"))
	if err != nil {
		return fmt.Errorf("read contracts version: %w", err)
	}
	var manifest struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return fmt.Errorf("parse contracts version: %w", err)
	}
	if manifest.Name != "jelto-contracts" || manifest.Version != required {
		return fmt.Errorf("contracts version mismatch: require jelto-contracts %s, found %s %s", required, manifest.Name, manifest.Version)
	}
	return nil
}

func run(args []string, stdout, stderr *os.File) error {
	flags := flag.NewFlagSet("runner", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var (
		root            = flags.String("root", "", "repository root (default: the module root found from the working directory)")
		hostBin         = flags.String("host", "", "conformance host binary (default: build spec/conformance/refhost)")
		mockBin         = flags.String("mockd", "", "mockd binary (default: build spec/conformance/mockd)")
		dir             = flags.String("scenarios", "", "scenario directory (default: <root>/spec/conformance/scenarios)")
		schema          = flags.String("schema", "", "wire schema (default: <root>/spec/wire-v1.schema.json)")
		contractVersion = flags.String("contracts-version", "", "require this version of the shared contracts package")
		only            = flags.String("only", "", "comma-separated C-numbers to run")
		keep            = flags.Bool("keep", false, "keep the working directory (state dirs, mockd record)")
		verbose         = flags.Bool("v", false, "print every passing check, not only the failures")
	)
	if err := flags.Parse(args); err != nil {
		return err
	}

	moduleRoot := *root
	if moduleRoot == "" {
		found, err := findModuleRoot()
		if err != nil {
			return err
		}
		moduleRoot = found
	}
	if *contractVersion != "" {
		if err := validateContractsVersion(moduleRoot, *contractVersion); err != nil {
			return err
		}
	}
	if *dir == "" {
		*dir = filepath.Join(moduleRoot, "spec", "conformance", "scenarios")
	}
	if *schema == "" {
		*schema = filepath.Join(moduleRoot, "spec", "wire-v1.schema.json")
	}

	scenarios, err := LoadScenarios(*dir)
	if err != nil {
		return err
	}
	if len(scenarios) == 0 {
		return fmt.Errorf("no scenarios in %s", *dir)
	}
	if *only != "" {
		wanted := map[string]bool{}
		for _, name := range strings.Split(*only, ",") {
			wanted[strings.TrimSpace(name)] = true
		}
		var filtered []Scenario
		for _, scenario := range scenarios {
			if wanted[scenario.C] {
				filtered = append(filtered, scenario)
			}
		}
		if len(filtered) == 0 {
			return fmt.Errorf("-only %q matched none of the %d scenarios in %s", *only, len(scenarios), *dir)
		}
		scenarios = filtered
	}

	validator, err := wirecheck.New(*schema)
	if err != nil {
		return err
	}

	work, err := os.MkdirTemp("", "jelto-conformance-")
	if err != nil {
		return err
	}
	if !*keep {
		defer func() { _ = os.RemoveAll(work) }()
	} else {
		fmt.Fprintf(stdout, "working directory: %s\n", work)
	}

	if *mockBin == "" {
		built, err := build(moduleRoot, work, "mockd", "./spec/conformance/mockd")
		if err != nil {
			return err
		}
		*mockBin = built
	}
	if *hostBin == "" {
		built, err := build(moduleRoot, work, "refhost", "./spec/conformance/refhost")
		if err != nil {
			return err
		}
		*hostBin = built
	}

	mockd, control, err := startMockd(*mockBin, work)
	if err != nil {
		return err
	}
	defer func() { _ = mockd.Process.Kill() }()

	mock, err := DialMock(control)
	if err != nil {
		return err
	}
	defer mock.Close()

	runner := &Runner{mock: mock, hostBin: *hostBin, validator: validator, workDir: work, out: stdout, verbose: *verbose}

	fmt.Fprintf(stdout, "spec/sdk-conformance.md §4 -- %d scenario(s), host %s\n\n", len(scenarios), filepath.Base(*hostBin))
	passed, failed, skipped := 0, 0, 0
	started := time.Now()
	for _, scenario := range scenarios {
		outcome := runner.Run(scenario)
		switch {
		case outcome.Skipped != "":
			skipped++
			fmt.Fprintf(stdout, "SKIP %-5s %s\n       %s\n", scenario.C, scenario.Title, indent(outcome.Skipped))
		case outcome.Passed():
			passed++
			fmt.Fprintf(stdout, "PASS %-5s %s\n", scenario.C, scenario.Title)
		default:
			failed++
			fmt.Fprintf(stdout, "FAIL %-5s %s\n", scenario.C, scenario.Title)
		}
		if outcome.Skipped != "" {
			continue
		}
		for _, arm := range outcome.Arms {
			if arm.Error != nil {
				fmt.Fprintf(stdout, "       arm %q could not run: %v\n", arm.Arm.Name, arm.Error)
			}
			for _, check := range arm.Results {
				if check.Passed && !*verbose {
					continue
				}
				marker := "  ok"
				if !check.Passed {
					marker = "FAIL"
				}
				fmt.Fprintf(stdout, "     %s [%s] %s: %s\n", marker, arm.Arm.Name, check.Name, indent(check.Detail))
			}
		}
	}
	fmt.Fprintf(stdout, "\n%d passed, %d failed, %d skipped in %s\n", passed, failed, skipped, time.Since(started).Round(time.Millisecond))
	mock.Shutdown()
	if failed > 0 {
		return fmt.Errorf("%d scenario(s) failed", failed)
	}
	return nil
}

func indent(text string) string {
	return strings.ReplaceAll(strings.TrimRight(text, "\n"), "\n", "\n     ")
}

func build(moduleRoot, work, name, pkg string) (string, error) {
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	output := filepath.Join(work, name)
	cmd := exec.Command("go", "build", "-o", output, pkg)
	cmd.Dir = moduleRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go build %s: %v\n%s", pkg, err, out)
	}
	return output, nil
}

// startMockd launches the endpoint and reads its one-line ready record, which
// is what stops a scenario racing the bind (mockd main.go, `Ready`).
func startMockd(binary, work string) (*exec.Cmd, string, error) {
	control := filepath.Join(work, "mockd.sock")
	cmd := exec.Command(binary,
		"-addr", "127.0.0.1:0",
		"-control", control,
		"-record", filepath.Join(work, "mockd-record.jsonl"),
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, "", err
	}
	logFile, err := os.Create(filepath.Join(work, "mockd.log"))
	if err != nil {
		return nil, "", err
	}
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		return nil, "", err
	}
	line, err := bufio.NewReader(stdout).ReadBytes('\n')
	if err != nil {
		return nil, "", fmt.Errorf("mockd did not print a ready line: %w", err)
	}
	var ready struct {
		Addr    string `json:"addr"`
		Control string `json:"control"`
	}
	if err := json.Unmarshal(line, &ready); err != nil {
		return nil, "", fmt.Errorf("mockd ready line %q: %w", strings.TrimSpace(string(line)), err)
	}
	go func() {
		buffer := make([]byte, 4096)
		for {
			if _, err := stdout.Read(buffer); err != nil {
				return
			}
		}
	}()
	return cmd, ready.Control, nil
}

func findModuleRoot() (string, error) {
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
			return "", fmt.Errorf("no go.mod above %s; pass -root", dir)
		}
		dir = parent
	}
}
