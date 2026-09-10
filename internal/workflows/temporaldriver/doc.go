//go:build temporal

// Package temporaldriver is the Temporal-backed driver for the durable
// workflow engine (issue #34 [O-25], ADR-0009).
//
// The whole package is gated behind the `temporal` build tag: default builds
// never compile any of this code, so the default engine path is untouched and
// the rollback story is "rebuild without the tag".
//
// Honest interface mapping (see docs/adr/0009-temporal-driver.md):
//
//   - internal/workflows exposes exactly ONE Go interface:
//     workflows.Services.QueueMessage — the message-queue port the engine's
//     step machine calls. Here it maps to a Temporal ACTIVITY
//     (Activities.QueueMessageActivity) executed with a bounded retry policy,
//     so workflow steps gain durable server-side retries.
//
//   - The engine's persistence is CONCRETE (*pgxpool.Pool + *outbox.Writer;
//     there is no storage interface to implement). The driver therefore
//     defines its own narrow audit port (Store) with two implementations:
//     NewPostgresStore writes the SAME workflow_instances/workflow_steps rows
//     and outbox events as the engine (schema from migration 0010), so the
//     existing read API (workflows.Engine.Get) observes Temporal-driven
//     instances unchanged; NewMemoryStore serves tests and execution-only
//     modes.
//
//   - The engine's deterministic step machine (start → wait → contact;
//     SMS → 48h → WhatsApp → 72h → escalate → close) maps 1:1 to Temporal
//     WORKFLOWS (CallbackWorkflow, CollectionsWorkflow) with durable timers,
//     replay-safe determinism, and step-recording activities.
//
// The driver does NOT fabricate engine behavior it does not have: it does not
// use Temporal signals/queries/updates/cron, and Driver.Get reads the audit
// store (Postgres), not Temporal state — Temporal-side state is available
// separately via Driver.TemporalStatus for operators.
//
// If Temporal is unreachable, starting workflows fails loudly
// (temporal.start_failed) — behavior is never silently degraded onto a
// second-class path.
package temporaldriver
