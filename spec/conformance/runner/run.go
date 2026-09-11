package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"jelto.io/jelto/contracts/spec/wirecheck"
)

// hostCommandTimeout bounds one §3 command. A `sleep 3600000` under a virtual
// clock settles in real milliseconds; a real one takes its own time, so the
// bound is generous and is only a wedge detector.
const hostCommandTimeout = 15 * time.Minute

// Runner executes scenarios against one mockd and one host binary.
type Runner struct {
	mock      *Mock
	hostBin   string
	validator *wirecheck.Validator
	workDir   string
	out       io.Writer
	verbose   bool
}

// ArmOutcome is what one arm produced.
type ArmOutcome struct {
	Arm     Arm
	Results []Result
	Error   error
}

type ScenarioOutcome struct {
	Scenario Scenario
	Arms     []ArmOutcome
	Skipped  string
}

func (o ScenarioOutcome) Passed() bool {
	if o.Skipped != "" {
		return false
	}
	for _, arm := range o.Arms {
		if arm.Error != nil {
			return false
		}
		for _, result := range arm.Results {
			if !result.Passed {
				return false
			}
		}
	}
	return true
}

// Run executes one scenario, arm by arm.
func (r *Runner) Run(scenario Scenario) ScenarioOutcome {
	outcome := ScenarioOutcome{Scenario: scenario}
	if scenario.Skip != "" {
		outcome.Skipped = scenario.Skip
		return outcome
	}
	stateDir := ""
	for i, arm := range scenario.Arms {
		if !arm.KeepState || stateDir == "" {
			stateDir = filepath.Join(r.workDir, fmt.Sprintf("%s-arm%d", scenario.C, i))
			if err := os.MkdirAll(stateDir, 0o700); err != nil {
				outcome.Arms = append(outcome.Arms, ArmOutcome{Arm: arm, Error: err})
				continue
			}
		}
		outcome.Arms = append(outcome.Arms, r.runArm(scenario, arm, stateDir))
	}
	return outcome
}

func (r *Runner) runArm(scenario Scenario, arm Arm, stateDir string) ArmOutcome {
	result := ArmOutcome{Arm: arm}

	if err := r.mock.Reset(); err != nil {
		result.Error = err
		return result
	}
	if len(arm.MockScript) > 0 {
		if err := r.mock.SetScript(arm.MockScript, arm.MockDefault); err != nil {
			result.Error = err
			return result
		}
	} else {
		mode := arm.Mock
		if mode == "" {
			// §4: "Every scenario starts with an empty JELTO_STATE_DIR and
			// mockd in `ok` mode unless stated."
			mode = "ok"
		}
		if err := r.mock.SetMode(mode); err != nil {
			result.Error = err
			return result
		}
	}

	env := map[string]string{
		"JELTO_ENDPOINT":  "http://" + r.mock.Addr() + "/v1/e",
		"JELTO_STATE_DIR": stateDir,
	}
	for key, value := range arm.Env {
		env[key] = value
	}
	if arm.MockClockFromNow {
		// mockd doc.go, "clock": a host under JELTO_NOW is in a different
		// frame, and an `until` in the host's past is a switch that was never
		// on.
		pin, err := strconv.ParseInt(env["JELTO_NOW"], 10, 64)
		if err != nil {
			result.Error = fmt.Errorf("mock_clock_from_now needs a JELTO_NOW that fits int64: %v", err)
			return result
		}
		if err := r.mock.SetClock(pin); err != nil {
			result.Error = err
			return result
		}
	}

	armStartWallMS := time.Now().UnixMilli()
	var (
		hosts     []*Host
		exitCodes []int
		exitMS    []int64
		replies   []HostReply
		stderr    strings.Builder

		// stepWallMS is the wall clock when each step finished, and
		// requestsAfterStep how many ingest requests mockd had recorded by
		// then. Together they are how a scenario says "the first batch arrives
		// 2 s after init" (C7) and "no request while the switch was on" (C16,
		// whose 60 s is in the host's simulated frame and so cannot be read off
		// mockd's monotonic clock at all).
		stepWallMS        []int64
		requestsAfterStep []int
		virtualNowMS      = int64(0)
		virtualClock      = false

		// stateExport is §1's fourth assertion surface: what the host printed
		// for `dumpstate` (§3.2). The LAST one captured wins -- state persists
		// across launches, so on an arm that restarts it is the final host's
		// view the state rows are about.
		stateExport json.RawMessage
	)
	if pin, ok := env["JELTO_NOW"]; ok {
		if parsed, err := strconv.ParseInt(pin, 10, 64); err == nil {
			virtualNowMS, virtualClock = parsed, true
		}
	}
	current, err := StartHost(r.hostBin, env)
	if err != nil {
		result.Error = err
		return result
	}
	hosts = append(hosts, current)

	stopCurrent := func() {
		if current == nil {
			return
		}
		// Capture state before graceful teardown: exit may flush and alter the state
		// the scenario reached. Missing exports fail state assertions rather than setup.
		if result.Error == nil {
			if reply, err := current.Send("dumpstate", 30*time.Second); err == nil && reply.OK && len(reply.State) > 0 {
				stateExport = append(json.RawMessage(nil), reply.State...)
			}
		}
		startedStop := time.Now()
		code, err := current.Stop(30 * time.Second)
		if err != nil && result.Error == nil {
			result.Error = err
		}
		exitMS = append(exitMS, time.Since(startedStop).Milliseconds())
		exitCodes = append(exitCodes, code)
		replies = append(replies, current.Replies()...)
		stderr.WriteString(current.Stderr())
		current = nil
	}

	// Abrupt kill captures no dumpstate or synthetic exit result: persistence must
	// precede the kill, and placeholder results would shift later scenario indexes.
	// Previously produced replies and stderr remain observable.
	killCurrent := func() {
		if current == nil {
			return
		}
		current.Kill()
		replies = append(replies, current.Replies()...)
		stderr.WriteString(current.Stderr())
		current = nil
	}

	mark := func() {
		stepWallMS = append(stepWallMS, time.Now().UnixMilli())
		count := 0
		if records, err := r.mock.Recording(); err == nil {
			count = len(Requests(records))
		}
		requestsAfterStep = append(requestsAfterStep, count)
	}

steps:
	for i, step := range arm.Steps {
		if virtualClock {
			for _, template := range step.Run {
				if fields := strings.Fields(template); len(fields) == 2 && fields[0] == "sleep" {
					if millis, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
						repeat := int64(step.Repeat)
						if repeat <= 0 {
							repeat = 1
						}
						virtualNowMS += millis * repeat
					}
				}
			}
		}
		switch {
		case len(step.Run) > 0:
			if current == nil {
				result.Error = fmt.Errorf("step %d: no host is running", i)
				break steps
			}
			repeat := step.Repeat
			if repeat <= 0 {
				repeat = 1
			}
			for n := 0; n < repeat; n++ {
				for _, template := range step.Run {
					// `{i}` is the iteration index, so C6's 1 500 distinctly
					// named tracks are one line of YAML and the queue can be
					// asserted to hold 501-1500 and not 1-500.
					command := strings.ReplaceAll(template, "{i}", strconv.Itoa(n))
					reply, err := current.Send(command, hostCommandTimeout)
					if err != nil {
						result.Error = fmt.Errorf("step %d: %w", i, err)
						break steps
					}
					if !reply.OK && reply.Error != "" {
						result.Error = fmt.Errorf("step %d: host refused %q: %s", i, command, reply.Error)
						break steps
					}
				}
			}
		case step.Await != nil:
			timeout := step.Await.TimeoutMS
			if timeout <= 0 {
				timeout = 15000
			}
			if err := r.mock.Await(step.Await.Count, timeout); err != nil {
				result.Error = fmt.Errorf("step %d: %w", i, err)
				break steps
			}
		case step.WaitMS > 0:
			time.Sleep(time.Duration(step.WaitMS) * time.Millisecond)
		case step.Mock != "":
			if err := r.mock.SetMode(step.Mock); err != nil {
				result.Error = fmt.Errorf("step %d: %w", i, err)
				break steps
			}
		case len(step.MockScript) > 0:
			if err := r.mock.SetScript(step.MockScript, step.MockDefault); err != nil {
				result.Error = fmt.Errorf("step %d: %w", i, err)
				break steps
			}
		case step.StopHost:
			stopCurrent()
		case step.KillHost:
			killCurrent()
		case step.Restart != nil:
			// fallthrough to the restart body below
			stopCurrent()
			next := map[string]string{}
			for key, value := range env {
				next[key] = value
			}
			for key, value := range step.Restart.Env {
				next[key] = value
			}
			env = next
			started, err := StartHost(r.hostBin, env)
			if err != nil {
				result.Error = fmt.Errorf("step %d: %w", i, err)
				break steps
			}
			current = started
			hosts = append(hosts, started)
		}
		mark()
	}
	stopCurrent()

	records, err := r.mock.Recording()
	if err != nil {
		if result.Error == nil {
			result.Error = err
		}
		return result
	}
	stats, err := r.mock.Stats()
	if err != nil && result.Error == nil {
		result.Error = err
	}

	evidence := Evidence{
		Scenario:          scenario,
		Arm:               arm,
		Records:           records,
		Requests:          Requests(records),
		Stderr:            stderr.String(),
		Replies:           replies,
		State:             stateExport,
		StateDir:          stateDir,
		ExitCodes:         exitCodes,
		ExitMS:            exitMS,
		StepWallMS:        stepWallMS,
		RequestsAfterStep: requestsAfterStep,
		VirtualNowMS:      virtualNowMS,
		VirtualClock:      virtualClock,
		ArmStartWallMS:    armStartWallMS,
		Stats:             stats,
		Validator:         r.validator,
	}
	result.Results = RunChecks(evidence)
	_ = hosts
	return result
}
