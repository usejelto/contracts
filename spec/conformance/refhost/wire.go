package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"runtime"
)

// The grammars of spec/wire-v1.md, each named for the field it guards. They
// are compiled from the document, not from the schema file: an SDK ships
// without spec/wire-v1.schema.json and must still refuse to send a body the
// schema would reject.
var (
	// §3 `n`.
	reEventName = regexp.MustCompile(`^[a-z0-9_:.-]{1,64}$`)
	// §3 `props` key.
	rePropKey = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)
	// §4 install-property value -- "one grammar, the one the server has always
	// applied to all of them", `license` included.
	reInstallPropValue = regexp.MustCompile(`^[a-z0-9_.-]{1,24}$`)
	// §4 onboarding `<step>` and `reason`.
	reOnboardingStep   = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)
	reOnboardingReason = regexp.MustCompile(`^[a-z0-9_.-]+$`)
	// §3 `v`, normative since rev 0.16.
	reClientVersion = regexp.MustCompile(`^[a-z]+/[0-9A-Za-z.+-]{1,24}$`)
	// §5.2 `a`.
	reAppSlug = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)
)

// Wire limits, all from spec/wire-v1.md §2 and §3.
const (
	maxBodyBytes    = 65_536 // §2, and W1's "body ≤ 64 KB"
	maxEventsPerReq = 100    // §2, and RFC-0001 §8.3 item 7's "Batch ≤ 100 events"
	maxProps        = 20     // §3, and §4's heartbeat cap since rev 0.16 (W3)
	maxPropString   = 200    // §3
	maxReasonChars  = 64     // §4
)

// Platform is the §5.2 app surface every app event carries. spec's schema
// requires iid/av/os/osv/arch on EVERY `s:"app"` event, not only the reserved
// ones, so this is built once and stamped on all of them.
type Platform struct {
	AppVersion string
	OS         string
	OSVersion  string
	Arch       string
	Slug       string // §5.2 `a`; "" is absent
}

// detectPlatform maps the Go runtime onto §5.2's closed enums. A platform §5.2
// does not name (freebsd, riscv64) has no legal value, so refhost refuses to
// invent one and reports it: sending `os:"freebsd"` would be an invalid_field
// on every event for the life of the build.
func detectPlatform(appVersion string) (Platform, error) {
	platform := Platform{AppVersion: appVersion, OSVersion: "0"}
	switch runtime.GOOS {
	case "darwin":
		platform.OS = "macos"
	case "windows":
		platform.OS = "windows"
	case "linux":
		platform.OS = "linux"
	default:
		return platform, fmt.Errorf("spec/wire-v1.md §5.2 `os` is macos|windows|linux; this build is %s", runtime.GOOS)
	}
	switch runtime.GOARCH {
	case "arm64":
		platform.Arch = "arm64"
	case "amd64":
		platform.Arch = "x64"
	case "386":
		platform.Arch = "x86"
	default:
		return platform, fmt.Errorf("spec/wire-v1.md §5.2 `arch` is arm64|x64|x86; this build is %s", runtime.GOARCH)
	}
	return platform, nil
}

// validateProps applies §3's `props` rules to a custom event's payload. A
// violation costs the EVENT, not the field: W3's `track x {...}` row says the
// call is "dropped client-side", and a server that received it would answer
// invalid_field for the whole event anyway.
func validateProps(props map[string]any) error {
	if len(props) > maxProps {
		return fmt.Errorf("props has %d keys, spec/wire-v1.md §3 caps them at %d", len(props), maxProps)
	}
	for key, value := range props {
		if !rePropKey.MatchString(key) {
			return fmt.Errorf("props key %q does not match spec/wire-v1.md §3's ^[a-z0-9_]{1,32}$", key)
		}
		switch typed := value.(type) {
		case string:
			if len(typed) > maxPropString {
				return fmt.Errorf("props value for %q is %d chars, spec/wire-v1.md §3 caps a string at %d", key, len(typed), maxPropString)
			}
		case bool, float64, int, int64, json.Number:
		default:
			return fmt.Errorf("props value for %q is %T; spec/wire-v1.md §3 allows a string, a number or a boolean", key, value)
		}
	}
	return nil
}

// buildEvent renders one queued event as the JSON object that goes on the
// wire. Install properties are resolved HERE, from the caller's snapshot,
// because §4 says a heartbeat carries the app's current values every time.
func buildEvent(event QueuedEvent, platform Platform, clientVersion string, installID string, installProps map[string]string) ([]byte, error) {
	if event.Metadata != nil {
		platform, clientVersion = event.Metadata.Platform, event.Metadata.Version
		if event.Metadata.InstallID != "" {
			installID = event.Metadata.InstallID
		}
	}
	av := []rune(platform.AppVersion)
	if len(av) > 32 {
		av = av[:32]
	}
	if len(av) == 0 {
		av = []rune("1.0.0")
	}
	fields := map[string]any{
		"id":   event.ID,
		"n":    event.N,
		"t":    event.T,
		"s":    "app",
		"iid":  installID,
		"av":   string(av),
		"os":   platform.OS,
		"osv":  platform.OSVersion,
		"arch": platform.Arch,
	}
	if platform.Slug != "" {
		fields["a"] = platform.Slug
	}
	if clientVersion != "" {
		fields["v"] = clientVersion
	}
	switch {
	case event.Heartbeat:
		if len(installProps) > 0 {
			props := make(map[string]any, len(installProps))
			for key, value := range installProps {
				props[key] = value
			}
			fields["props"] = props
		}
	case len(event.Props) > 0:
		fields["props"] = event.Props
	}
	return json.Marshal(fields)
}

// buildEnvelope packs as many rendered events as §2 allows -- 100 of them, and
// 65 536 bytes -- and reports how many it took. It is assembled by hand so the
// byte budget is measured on the bytes that are actually sent.
func buildEnvelope(productKey string, events [][]byte) (body []byte, used int) {
	head := []byte(`{"v":1,"p":` + quote(productKey) + `,"e":[`)
	var buffer bytes.Buffer
	buffer.Write(head)
	for i, event := range events {
		if i >= maxEventsPerReq {
			break
		}
		extra := len(event)
		if i > 0 {
			extra++ // the comma
		}
		if buffer.Len()+extra+2 > maxBodyBytes && i > 0 {
			break
		}
		if i > 0 {
			buffer.WriteByte(',')
		}
		buffer.Write(event)
		used++
	}
	buffer.WriteString(`]}`)
	return buffer.Bytes(), used
}

func quote(value string) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return `""`
	}
	return string(raw)
}

// ingestResponse is the part of a 202 body an SDK acts on: spec/wire-v1.md §6
// and §8. Nothing else in the body is read, and an unreadable body is a
// swallowed failure (§8.3 item 10), never a retry.
type ingestResponse struct {
	Rejected []struct {
		I      int    `json:"i"`
		Reason string `json:"reason"`
		Field  string `json:"field"`
	} `json:"rejected"`
	Stop *struct {
		Until json.Number `json:"until"`
		Scope string      `json:"scope"`
	} `json:"stop"`
	Error string `json:"error"`
}

func decodeResponse(body []byte) (ingestResponse, error) {
	var parsed ingestResponse
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&parsed); err != nil {
		return ingestResponse{}, err
	}
	return parsed, nil
}
