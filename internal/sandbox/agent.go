package sandbox

// The agent is the binary that serves the sandbox contract — exec and files —
// inside the sandbox itself. Every backend copies it out of the image that
// publishes it and into the workspace, which is what lets ANY runtime image be
// a sandbox image: the image serves nothing, the agent does.
//
// The three values below are one fact split across a process boundary — where
// the binary is in the publishing image, where it lands in the workspace, and
// which published version — so both backends read them here. A mismatch would
// not fail to compile; it would be a sandbox that starts and cannot exec.
const (
	// AgentImage publishes the reference agent, pinned by tag: the tag IS the
	// version, and an operator may pin harder with a digest. The Helm chart
	// carries the same pin in values.yaml (sandboxes.agentImage) — bump both.
	AgentImage = "ghcr.io/open-runtimes/sandbox:0.1.0"
	// AgentSource is the binary's path inside AgentImage.
	AgentSource = "/usr/local/bin/sandbox"
	// AgentName is the copy's filename. It sits at the workspace root so the
	// copy needs no mkdir, and therefore no shell in the publishing image.
	AgentName = ".sandbox-agent"
)

// AgentPath is where the agent lands in a workspace, and therefore the command a
// sandbox runs unless its pool or its request names another.
func AgentPath(workspace string) string { return workspace + "/" + AgentName }
