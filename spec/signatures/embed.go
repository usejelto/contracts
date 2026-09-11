// Package signatures exposes normative signature data embedded beside its
// single source of truth. It contains no classification logic; read-side bot
// and source accessors arrive with the query engine.
package signatures

import _ "embed"

//go:embed referrer-spam.txt
var referrerSpam []byte

//go:embed referrer-spam-read.txt
var referrerSpamRead []byte

//go:embed bots.yaml
var bots []byte

//go:embed sources.yaml
var sources []byte

// ReferrerSpam returns a copy of the ingest-time referrer spam list: the short
// hand-reviewed one, whose hosts are rejected at write (spec/wire-v1.md §6).
func ReferrerSpam() []byte { return append([]byte(nil), referrerSpam...) }

// ReferrerSpamRead returns a copy of the generated on-read referrer spam table
// (spec/metrics.md §6b). It is the large one, and it never rejects anything:
// classification is on read, where growing the table reclassifies history.
func ReferrerSpamRead() []byte { return append([]byte(nil), referrerSpamRead...) }

// Bots returns a copy of the normative on-read bot signature table.
func Bots() []byte { return append([]byte(nil), bots...) }

// Sources returns a copy of the normative on-read source signature table.
func Sources() []byte { return append([]byte(nil), sources...) }
