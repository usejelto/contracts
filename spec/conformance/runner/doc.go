// Command runner drives conformance hosts through spec/sdk-conformance.md §4 scenarios.
// It asserts only on mock recordings, host replies and exits, stderr, exported
// state, and state-directory emptiness. SDK storage formats remain private.
//
// Run all scenarios, selected scenarios, or a specific SDK host:
//
//	go run ./spec/conformance/runner
//	go run ./spec/conformance/runner -only C8,C16
//	go run ./spec/conformance/runner -host /absolute/path/to/sdk/conformance-host
//
// Without -host, the runner builds and drives spec/conformance/refhost.
//
// # Scenario format
//
// Each YAML file in scenarios/ contains named arms. An arm defines its mock
// configuration, environment, ordered steps, and assertions. It starts with a
// fresh recording and a fresh or explicitly retained state directory. Available
// checks are defined in checks.go; scenarios cannot execute arbitrary code.
//
// W1, W4, and mock_errors run on every arm: all requests must satisfy wire
// schema, content type, batch limits, and client-version rules, and mockd must
// have honored the requested behavior.
//
// # State and numeric fidelity
//
// State checks use the last dumpstate reply captured at graceful teardown.
// state_dir_empty reads the directory itself: an export cannot prove that no
// forgotten files remain. Abrupt kill captures no state or synthetic exit code.
//
// Record timestamps use json.RawMessage, and schema validation uses UseNumber.
// Do not decode wire numbers through float64: C15b requires exact large values
// and exponent literals.
//
// The report distinguishes PASS, FAIL, and SKIP, with a reason for every skip.
package main
