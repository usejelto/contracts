package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Exercise the same build-and-launch path used for mockd and refhost. Windows
// requires the .exe suffix even when go build writes to an explicit output path.
func TestBuiltToolCanBeExecuted(t *testing.T) {
	module := t.TempDir()
	for name, source := range map[string]string{
		"go.mod":  "module example.com/conformance-probe\n\ngo 1.25\n",
		"main.go": "package main\nimport \"fmt\"\nfunc main() { fmt.Print(\"ready\") }\n",
	} {
		if err := os.WriteFile(filepath.Join(module, name), []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	binary, err := build(module, t.TempDir(), "probe", ".")
	if err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command(binary).CombinedOutput()
	if err != nil {
		t.Fatalf("launch built tool: %v\n%s", err, output)
	}
	if string(output) != "ready" {
		t.Fatalf("tool output = %q, want ready", output)
	}
}
