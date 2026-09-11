// Package wirecheck shares wire-schema validation and debug-log classification
// between conformance scenarios and captured SDK traffic.
package wirecheck

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Validator applies W1 against the current schema loaded from the repository.
type Validator struct {
	schema *jsonschema.Schema
	path   string
}

// JSON Schema cannot enforce the encoded body-size or content-type limits.
const (
	MaxBodyBytes = 65_536
	MaxEvents    = 100
)

// New compiles the schema at path. Callers pass spec/wire-v1.schema.json.
func New(path string) (*Validator, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	if err := compiler.AddResource("https://jelto.io/spec/wire-v1.schema.json", document); err != nil {
		return nil, err
	}
	schema, err := compiler.Compile("https://jelto.io/spec/wire-v1.schema.json")
	if err != nil {
		return nil, fmt.Errorf("compile %s: %w", path, err)
	}
	return &Validator{schema: schema, path: path}, nil
}

// Path is the schema file this validator was compiled from, for error messages
// that have to name the contract they measured against.
func (v *Validator) Path() string { return v.path }

// ValidateBody preserves numeric literals with UseNumber. Whole-number fields
// may use fraction or exponent syntax when their numeric value is integral.
func (v *Validator) ValidateBody(body []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return fmt.Errorf("body is not JSON: %w", err)
	}
	if err := v.schema.Validate(value); err != nil {
		return err
	}
	return nil
}
