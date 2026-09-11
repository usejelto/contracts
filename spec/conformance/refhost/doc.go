// Command refhost implements the SDK behavioral contract for testing the
// conformance harness. It is not a customer SDK and does not claim platform
// integration, crash-safety, or the SDK memory budget.
//
// Behavior follows RFC-0001 §8 and spec/wire-v1.md. NO RULE comments identify
// choices the contract does not settle.
//
// # Command protocol
//
// Commands follow spec/sdk-conformance.md §3. Each produces one JSON reply on
// stdout after completion, with cmd echoing the command. SDK diagnostics go to
// stderr. installid returns value, dumpstate returns state, and init reports us.
// JELTO_CLIENT_VERSION overrides the default, including with an empty value.
// JELTO_SEED is for manual reproduction only; scenarios must retain real randomness.
//
// # Clock and synchronization
//
// JELTO_NOW pins an arbitrary-precision millisecond clock. sleep advances that
// clock and waits for due work; without a pin it sleeps in real time. Network
// timeouts always use real time.
//
// Virtual sleep waits for the pump's idle acknowledgement before and after
// advancing time. The first barrier prevents bootstrap from anchoring deadlines
// to the advanced clock. The pump reads the barrier before the clock and replies
// only after a complete evaluation finds nothing due, including completed network
// work. Both barriers share a 30-second real-time budget. Quiet polling cannot
// prove that a scheduled pump has run (TODO.md §5).
//
// # State
//
// State and queue files are created lazily after init. dumpstate exports semantic
// facts under §3.2 without computing missing values. Scenarios may inspect that
// export and directory emptiness, but cannot depend on storage names or formats.
// The filesystem check is necessary because an export cannot reveal forgotten files.
package main
