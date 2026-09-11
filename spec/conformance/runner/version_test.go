package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestContractsVersionRejectsMissingInvalidAndIncompatiblePackages(t *testing.T) {
	root := t.TempDir()
	if err := validateContractsVersion(root, "0.1.0"); err == nil {
		t.Fatal("missing contracts manifest accepted")
	}
	dir := filepath.Join(root, "spec", "contracts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		raw   string
		valid bool
	}{
		{`{"name":"jelto-contracts","version":"0.1.0"}`, true},
		{`{"name":"jelto-contracts","version":"0.2.0"}`, false},
		{`{"name":"unrelated","version":"0.1.0"}`, false},
		{`not json`, false},
	} {
		if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(tc.raw), 0o644); err != nil {
			t.Fatal(err)
		}
		err := validateContractsVersion(root, "0.1.0")
		if tc.valid && err != nil {
			t.Fatal(err)
		}
		if !tc.valid && (err == nil || !strings.Contains(err.Error(), "contracts version")) {
			t.Fatalf("invalid manifest: %v", err)
		}
	}
}
