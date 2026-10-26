package main

import "go.kenn.io/kata/internal/agentguide"

// agentsBlockBegin and agentsBlockEnd delimit the section kata manages inside
// an agent guidance file. They let init refresh kata's guidance in place across
// re-runs without disturbing anything else a project keeps in that file.
const (
	agentsBlockBegin = "<!-- BEGIN KATA (managed by `kata init --with-agents`) -->"
	agentsBlockEnd   = "<!-- END KATA -->"
)

// agentContractText is the canonical marker-free briefing shared by dynamic
// session injection and repository-managed agent guidance.
const agentContractText = agentguide.Contract

// agentsManagedBlock returns the full marker-delimited block kata writes.
func agentsManagedBlock() string {
	return agentsBlockBegin + "\n" + agentContractText + agentsBlockEnd
}
