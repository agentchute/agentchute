// Package op is the operation seam: the ONLY way the CLI (and, from M3, the
// hub session dispatcher) mutates or reads pool state.
//
// Layering (DESIGN.md §7.4, B2). `internal/cli` parses flags and renders;
// `internal/op` owns validate-and-mutate; `internal/loop` keeps the
// primitives. The dependency direction is one-way and enforced by a test
// (deps_test.go): op imports ONLY the standard library and `internal/loop`.
// It must never import `internal/cli` — every helper an op needs MOVED here
// rather than staying behind a callback, precisely because a call back into
// the CLI would close a cycle.
//
// Conventions (DESIGN.md §3):
//
//   - Actor context (C1). Actor-scoped ops take an explicit, NON-WIRE
//     Context; neither loop.Config nor any request struct carries identity.
//     The local CLI builds it from resolveAgentID; the hub session builds it
//     once from the forced command's pinned --agent.
//   - Wire shape. Requests/responses are plain JSON-taggable structs — they
//     are also the wire payloads (§4.4) — and errors are typed sentinels
//     mapped 1:1 to wire error codes (§4.4.2, CodeFor).
//   - Streaming producers. Ops whose results are unbounded (Claim, Ack,
//     Pending, Status) take a single typed emitter and never materialize a
//     result slice; terminal summaries carry counts only.
package op

// Context is the actor an operation runs as. Deliberately NOT part of any
// request struct and never serialized: identity arrives out-of-band (locally
// from the CLI's own resolution, remotely from the forced command's pinned
// key id, §5.3), so a request can never assert a second, contradictory actor.
type Context struct {
	ActorID string
}
