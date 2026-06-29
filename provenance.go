package main

import (
	"os"

	"github.com/agentchute/agentchute/internal/loop"
)

// hookLaunchProvenance returns the launch provenance for a lifecycle-hook enroll
// (boot / self-check). Normally this is LaunchedByHook plus the hook event. But
// when the hook fires INSIDE the runner (the runner exports AGENTCHUTE_RUNNER=1
// to its child wrapper), the runner owns the lane — so we record LaunchedByRunner
// and drop the hook event, so a runner-launched wrapper's SessionStart hook does
// not demote the truthful "runner" provenance to "hook".
func hookLaunchProvenance(event string) (launchedBy, hookEvent string) {
	if os.Getenv("AGENTCHUTE_RUNNER") == "1" {
		return loop.LaunchedByRunner, ""
	}
	return loop.LaunchedByHook, event
}

// wrapperCandidatesForAgent returns the real wrapper-binary names for the given
// agent id (matched by canonical base, so contextual ids like
// gemini-cli-agentchute resolve), used by the launch-bypass shadowing probe.
// With no/unknown agent id it returns every known candidate.
func wrapperCandidatesForAgent(agentID string) []string {
	if agentID != "" {
		for _, spec := range shimSpecs {
			if registrationMatchesCanonical(agentID, spec.AgentID) {
				return spec.Candidates
			}
		}
	}
	var all []string
	for _, spec := range shimSpecs {
		all = append(all, spec.Candidates...)
	}
	return all
}
