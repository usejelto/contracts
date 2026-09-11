package signatures

import (
	"os"
	"strings"
	"testing"
)

// refreshTarget reads the make target out of a `# Refresh:  make signatures ...`
// header line.
func refreshTarget(line string) string {
	fields := strings.Fields(strings.TrimPrefix(line, "# Refresh:"))
	if len(fields) < 2 || fields[0] != "make" {
		return ""
	}
	return fields[1]
}

func commentHeader(data []byte) string {
	var header []string
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "#") {
			break
		}
		header = append(header, line)
	}
	return strings.Join(header, "\n")
}

// A table nobody can date is a table nobody can tell is stale (F30). This guard
// moved here with the split: the signature data, the refresh target and the
// Makefile that defines it all live in this repository, so a consumer asserting
// against its own Makefile would be checking the wrong file.
func TestSignatureTablesNameARefreshTargetThisRepositoryDefines(t *testing.T) {
	makefile, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	// sources.yaml is deliberately absent: it is hand-curated with no upstream to pin.
	for file, read := range map[string]func() []byte{
		"bots.yaml": Bots, "referrer-spam.txt": ReferrerSpam,
		"referrer-spam-read.txt": ReferrerSpamRead,
	} {
		header := commentHeader(read())
		for _, key := range []string{"# Upstream:", "# Pinned:", "# Refresh:"} {
			if !strings.Contains(header, key) {
				t.Errorf("%s: header carries no %q line", file, key)
			}
		}
		for _, line := range strings.Split(header, "\n") {
			if !strings.HasPrefix(line, "# Refresh:") {
				continue
			}
			target := refreshTarget(line)
			if target == "" {
				t.Errorf("%s: %q names no make target", file, line)
				continue
			}
			if !strings.Contains(string(makefile), "\n"+target+":") {
				t.Errorf("%s: header names `make %s`, which this repository's Makefile does not define",
					file, target)
			}
		}
	}
}
