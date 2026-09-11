package contracts

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

func TestDocumentsTravelWithModule(t *testing.T) {
	for name, read := range map[string]func() []byte{
		"spec/wire-v1.md": WireSpec, "spec/wire-v1.schema.json": WireSchema,
		"spec/sdk-conformance.md": SDKConformanceSpec,
	} {
		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		copy := read()
		if len(copy) == 0 || !bytes.Equal(copy, source) {
			t.Fatalf("%s: embedded document differs from source", name)
		}
		copy[0] ^= 0xff
		if !bytes.Equal(read(), source) {
			t.Fatalf("%s: caller mutated the shared document", name)
		}
	}
	var schema struct {
		ID string `json:"$id"`
	}
	if err := json.Unmarshal(WireSchema(), &schema); err != nil {
		t.Fatal(err)
	}
	if schema.ID != WireSchemaID {
		t.Fatalf("schema ID = %q; consumers register %q", schema.ID, WireSchemaID)
	}
}
