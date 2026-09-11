// Package contracts exposes the versioned protocol documents to Go consumers.
// The schema and specifications travel with the module and require no checkout
// paths at runtime or during downstream verification.
package contracts

import _ "embed"

// WireSchemaID is the stable resource identifier declared by the wire schema.
const WireSchemaID = "https://jelto.io/spec/wire-v1.schema.json"

//go:embed spec/wire-v1.md
var wireSpec string

//go:embed spec/wire-v1.schema.json
var wireSchema string

//go:embed spec/sdk-conformance.md
var sdkConformanceSpec string

// WireSpec returns an independent copy of the normative wire specification.
func WireSpec() []byte { return []byte(wireSpec) }

// WireSchema returns an independent copy of the executable wire schema.
func WireSchema() []byte { return []byte(wireSchema) }

// SDKConformanceSpec returns an independent copy of the SDK behavior contract.
func SDKConformanceSpec() []byte { return []byte(sdkConformanceSpec) }
