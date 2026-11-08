// Package privdrop defines the identity that agent-spawned child processes run
// under, and the helper that drops a child to it.
//
// Why it exists: in the container the platform process runs as root (the base
// image defaults to root), so agent-spawned children inherit that identity.
// The platform's own credential files — a docker secret mounted at
// /run/secrets/pg_dsn, mode 0600 but fully readable by root — are then readable
// by the agent, and the agent holding the connection string can bypass every
// application-layer scope check and talk to the database directly. Dropping
// tool children to a dedicated unprivileged UID breaks that read path.
//
// Why 65534 (nobody): it is the last unprivileged UID that essentially every
// base image ships. Not 0 — that is the platform process's own identity, so
// dropping to it is no drop at all. Not 1000 — the mounted db credential file
// in the container is owned by uid 1000 with mode 0600, so a child running as
// 1000 still reads it and the drop is theatre.
//
// The UID/GID live here so that there is exactly one source of truth: the host
// application derives its own agent identity from these constants instead of
// redeclaring them.
package privdrop

const (
	// AgentUID is the dedicated unprivileged UID for agent-spawned children.
	AgentUID = 65534
	// AgentGID is the dedicated unprivileged GID for agent-spawned children;
	// it has the same value as AgentUID.
	AgentGID = 65534
)
