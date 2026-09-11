package wirecheck

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// Census is what a week of real traffic actually put on the wire.
//
// It checks the coverage that validation alone cannot:
// the schema says every body was legal, and the census says WHICH FIELDS were
// exercised. A field nobody sent is a field nobody proved, and after the freeze
// its grammar is fixed either way -- so "not seen" is a finding, not a blank.
type Census struct {
	Bodies       int
	Events       int
	LargestBody  int
	MostEvents   int
	FieldsBySurf map[string]map[string]int // surface -> wire field -> count
	EventNames   map[string]int
	PropKeys     map[string]int
	ProductKeys  map[string]int
	ClientVers   map[string]int
}

func NewCensus() *Census {
	return &Census{
		FieldsBySurf: map[string]map[string]int{},
		EventNames:   map[string]int{},
		PropKeys:     map[string]int{},
		ProductKeys:  map[string]int{},
		ClientVers:   map[string]int{},
	}
}

// envelope and event are decoded with json.RawMessage for `e` so a body that
// fails the schema can still be counted field by field -- the census describes
// what a real client emitted, including when what it emitted was wrong.
type envelope struct {
	P string            `json:"p"`
	E []json.RawMessage `json:"e"`
}

// Add folds one body into the census. It returns an error only when the body is
// not JSON at all; a body that is JSON but not schema-valid is still counted,
// because the census is a record of what was sent, not of what was accepted.
func (c *Census) Add(body []byte) error {
	c.Bodies++
	if len(body) > c.LargestBody {
		c.LargestBody = len(body)
	}

	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var env envelope
	if err := decoder.Decode(&env); err != nil {
		return fmt.Errorf("body is not JSON: %w", err)
	}
	if env.P != "" {
		c.ProductKeys[env.P]++
	}
	if len(env.E) > c.MostEvents {
		c.MostEvents = len(env.E)
	}

	for _, raw := range env.E {
		c.Events++
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			continue
		}
		surface := "(no s)"
		if s, ok := fields["s"]; ok {
			var value string
			if json.Unmarshal(s, &value) == nil {
				surface = value
			}
		}
		if c.FieldsBySurf[surface] == nil {
			c.FieldsBySurf[surface] = map[string]int{}
		}
		for key := range fields {
			c.FieldsBySurf[surface][key]++
		}
		if n, ok := fields["n"]; ok {
			var value string
			if json.Unmarshal(n, &value) == nil {
				c.EventNames[value]++
			}
		}
		if v, ok := fields["v"]; ok {
			var value string
			if json.Unmarshal(v, &value) == nil {
				c.ClientVers[value]++
			}
		}
		if p, ok := fields["props"]; ok {
			var props map[string]json.RawMessage
			if json.Unmarshal(p, &props) == nil {
				for key := range props {
					c.PropKeys[key]++
				}
			}
		}
	}
	return nil
}

// wireFields is every field spec/wire-v1.md defines for a surface, so the report
// can name what was NOT seen rather than only listing what was. §3 is common to
// both surfaces; §5.1 is web; §5.2 is app.
var wireFields = map[string][]string{
	"common": {"id", "n", "t", "s", "props", "i", "v", "l"},
	"web":    {"u", "r", "w", "h", "f", "fd", "pv", "e", "sd", "vid"},
	"app":    {"iid", "av", "os", "osv", "arch", "a"},
}

// Unseen returns the fields spec/wire-v1.md defines for `surface` that the
// capture never carried, in the document's own order.
func (c *Census) Unseen(surface string) []string {
	seen := c.FieldsBySurf[surface]
	var missing []string
	for _, group := range []string{"common", surface} {
		for _, field := range wireFields[group] {
			if seen[field] == 0 {
				missing = append(missing, field)
			}
		}
	}
	return missing
}

// Sorted renders a count map as "key n · key n", descending by count then by
// key, so two runs over the same capture print identical bytes.
func Sorted(counts map[string]int) string {
	type pair struct {
		key   string
		count int
	}
	pairs := make([]pair, 0, len(counts))
	for key, count := range counts {
		pairs = append(pairs, pair{key, count})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].count != pairs[j].count {
			return pairs[i].count > pairs[j].count
		}
		return pairs[i].key < pairs[j].key
	})
	out := ""
	for i, p := range pairs {
		if i > 0 {
			out += " · "
		}
		out += fmt.Sprintf("%s %d", p.key, p.count)
	}
	if out == "" {
		return "(none)"
	}
	return out
}
