// Package work implements the durable GitHub-backed Work domain.
//
// Protocol parsing, history reconstruction, and dispatch eligibility are pure
// domain operations. GitHub transport and trust lookups are kept at the
// package boundary so the durable history never depends on localstate.
package work
