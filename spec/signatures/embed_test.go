package signatures

import (
	"bytes"
	"os"
	"testing"
)

func TestEmbeddedTablesMatchNormativeSources(t *testing.T) {
	for name, read := range map[string]func() []byte{
		"bots.yaml": Bots, "sources.yaml": Sources,
		"referrer-spam.txt": ReferrerSpam, "referrer-spam-read.txt": ReferrerSpamRead,
	} {
		t.Run(name, func(t *testing.T) {
			source, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			embedded := read()
			if len(embedded) == 0 || !bytes.Equal(embedded, source) {
				t.Fatal("embedded signature table differs from its normative source")
			}
			embedded[0] ^= 0xff
			if !bytes.Equal(read(), source) {
				t.Fatal("a caller can mutate the shared signature table")
			}
		})
	}
}
