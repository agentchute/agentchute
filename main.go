// agentchute — pull-only, inbox-based agent coordination via markdown files.
// Senders only write a recipient's inbox; nobody pokes a recipient. A loopless
// wrapper is supervised by the runner (`agentchute serve`), a per-agent PTY
// supervisor that polls the agent's own inbox and injects a `check inbox` cue
// (see AGENTCHUTE.md §8).
//
// See AGENTCHUTE.md (at repo root) for the full spec. This binary is the
// reference implementation of the optional CLI sketched in the spec. The
// protocol itself does not require this CLI; two agents can coordinate using
// nothing more than `ln`/`mv` over a shared inbox if they follow the spec.
//
// This file is a thin wiring layer: all command logic lives in internal/cli.
// The //go:embed assets below stay here because //go:embed cannot reference a
// parent directory (the spec, templates, and hooks live at the repo root), so
// they are embedded here and injected into cli.Main via cli.Assets.
package main

import (
	"embed"
	"os"

	"github.com/agentchute/agentchute/internal/cli"
)

// version is set at build time via -ldflags "-X main.version=..." (see
// .goreleaser.yaml). It is forwarded to the CLI through cli.Assets.
var version = "dev"

//go:embed AGENTCHUTE.md
var spec string

//go:embed templates/enrollment/wrapper.md
var wrapperTemplate string

//go:embed templates/enrollment/agents.md
var agentsTemplate string

//go:embed all:examples/hooks
var hooks embed.FS

func main() {
	os.Exit(cli.Main(cli.Assets{
		Version:         version,
		Spec:            spec,
		WrapperTemplate: wrapperTemplate,
		AgentsTemplate:  agentsTemplate,
		Hooks:           hooks,
	}, os.Args[1:]))
}
