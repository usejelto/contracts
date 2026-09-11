// Command mockd is the scriptable ingest endpoint for SDK conformance tests.
// It implements spec/sdk-conformance.md §2 with response bodies from
// spec/wire-v1.md §2a, independently of the production server. JSON responses do
// not include a trailing newline; clients must accept either form.
//
// # Modes
//
// A mode contains + joined clauses. At most one clause selects a status, except
// reject and stop can compose into one 202 body. down must stand alone.
// Invalid configuration fails at the control socket or returns 500 with X-Mock-Error.
//
//	ok                     202 {}
//	reject:<reason>        202 with one rejected entry per event
//	reject:<r>:<field>     rejected entries also include field
//	429 | 503             that status, Retry-After: 2, body {}
//	429:<v> | 503:<v>      literal Retry-After value; empty <v> omits the header
//	400                    400 {"error":"malformed"}
//	400:<code>             another wire §2a envelope error
//	402                    402 {"error":"payment_required"}
//	stop:<seconds>         202 with an app-scoped stop until now + seconds
//	stop:<seconds>:<scope> selects the stop scope (app or web)
//	slow:<ms>              delays the response; alone, delays ok
//	down                   closes the HTTP listener, refusing connections
//	<status>               any three-digit status; its wire body or {} if unspecified
//	garbage                202 application/json with an invalid JSON body
//	huge[:<bytes>]         202 with valid JSON padded past bytes (default 8 MiB)
//	hangup                 records the request and closes without responding
//
// Retry-After values are never clamped: C8c tests the client's ceiling.
// For C16 use stop:60+reject:stopped; stop alone accepts the batch without rejections.
//
// # Mode precedence
//
// Each request uses its non-empty X-Mock header, then the next script step, then
// the default mode. A header override does not consume a script step. mode_source
// records the selected channel. down must be configured through the control socket:
// it refuses connections before any request header exists.
//
// # Control socket
//
// -control selects a Unix stream socket (default <tmp>/mockd-<pid>.sock), also
// reported in the ready line. Its directory is 0700 and socket 0600: it exposes
// recorded bodies and controls endpoint behavior, so it must remain local.
//
// Framing is JSON commands in, one JSON reply per line out, ordered per connection.
// Commands serialize under one mutex. Replies contain ok:true or ok:false with error;
// malformed commands do not close the connection.
//
//	{"cmd":"ping"}
//	    Returns pid, addr, control, and uptime_ms.
//	{"cmd":"mode","mode":"429:20"}
//	    Sets the default mode and clears the script; validates at setup.
//	{"cmd":"script","steps":[{"mode":"429:2","times":6}],"default":"ok"}
//	    Consumes one step per request; times defaults to 1. On exhaustion,
//	    default becomes active, or the existing default remains if omitted.
//	    Empty steps clear the script. Scripting avoids racing mode changes against retries.
//	{"cmd":"clock","unix_ms":N}
//	    Pins stop's clock, advancing with real time from the pin. Zero unpins it.
//	    Keep this clock in the host's frame when testing stop.until.
//	{"cmd":"recording","since":0,"limit":0}
//	    Returns records with seq >= since; zero starts at the beginning.
//	    limit=0 returns all. next is the cursor for the following call; dropped reports loss.
//	{"cmd":"await","count":1,"since":0,"timeout_ms":8000}
//	    Waits for count request records (connections do not count), then returns
//	    records and next. Timeout returns ok:false with the available records.
//	{"cmd":"reset"}
//	    Clears recording and the record file, resets seq to 1, clears the script,
//	    selects ok, unpins the clock, and restores the listener atomically.
//	{"cmd":"stats"}
//	    Returns requests, connections, mock_errors, dropped, mode, script_remaining,
//	    and listening. Connections count accepted sockets even without a request (C5).
//	{"cmd":"shutdown"}
//	    Replies ok:true, then exits 0.
//
// # Recording
//
// Records include accepted connections and every request, including wrong paths,
// wrong methods, and OPTIONS. seq gives arrival order across both kinds; the record
// file is written on response completion, so sort by seq when reading it.
//
// conn_id identifies connection reuse. at and at_unix_ms are wall-clock values;
// since_start_us and responded_us are monotonic. Measure retry delays from response
// completion so slow responses and wall-clock adjustments do not corrupt schedules.
//
// Headers preserve all values. body holds raw UTF-8 bytes, or body_base64 holds
// non-UTF-8 data. body_bytes is the original length even when max-body truncates
// the stored copy; body_truncated marks that case. envelope summarizes events,
// but raw bytes remain authoritative and envelope_error explains a missing summary.
//
// Keep event t as json.RawMessage or use json.Decoder.UseNumber. float64 decoding
// would change exponent literals and large integers required by C15b.
//
// mode and mode_source describe configuration; response records what the client
// actually received, including status, headers, body, and delay_us. It is null
// until completion. mock_error must remain empty; stats.mock_errors is the aggregate.
//
// # Limits
//
// down cannot record refused connection attempts. Assert those through host state
// or diagnostics, or use hangup when a recorded network error is sufficient.
// hangup is a mid-exchange reset rather than a refusal; both follow the SDK network-error rule.
package main
