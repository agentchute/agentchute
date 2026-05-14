package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

func cmdSend(args []string) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var fromID, toID, taskField, statusField, body, replyTo, controlRepo, loopDir string
	fs.StringVar(&fromID, "from", "", "sender agent id (or $AGENTCHUTE_AGENT_ID)")
	fs.StringVar(&toID, "to", "", "recipient agent id")
	fs.StringVar(&taskField, "task", "", "short task descriptor for the message frontmatter (recommended)")
	fs.StringVar(&statusField, "status", "", "message status frontmatter field (e.g., request, signoff, info)")
	fs.StringVar(&body, "body", "", "message body markdown; if empty, body is read from stdin")
	fs.StringVar(&replyTo, "reply-to", "", "prior message_id this is replying to")
	fs.StringVar(&controlRepo, "control-repo", "", "control repo path (or AGENTCHUTE_CONTROL_REPO)")
	fs.StringVar(&loopDir, "loop-dir", "", "loop dir path (or AGENTCHUTE_LOOP_DIR)")

	if err := fs.Parse(args); err != nil {
		return sendUsage(err)
	}
	if fs.NArg() != 0 {
		return sendUsage(fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " ")))
	}

	fromID = strings.TrimSpace(firstNonEmpty(fromID, os.Getenv("AGENTCHUTE_AGENT_ID")))
	if fromID == "" {
		return fmt.Errorf("missing --from; pass explicitly or set AGENTCHUTE_AGENT_ID")
	}
	if err := loop.ValidateAgentID(fromID); err != nil {
		return fmt.Errorf("--from: %w", err)
	}
	toID = strings.TrimSpace(toID)
	if toID == "" {
		return fmt.Errorf("missing --to (recipient agent id)")
	}
	if err := loop.ValidateAgentID(toID); err != nil {
		return fmt.Errorf("--to: %w", err)
	}

	// Keep short-string flags one-line even though loop.ComposeMessage quotes
	// YAML-sensitive scalars. These fields are meant to be compact metadata.
	for _, fld := range []struct{ name, val string }{
		{"--task", taskField},
		{"--status", statusField},
		{"--reply-to", replyTo},
	} {
		if err := rejectFrontmatterInjection(fld.name, fld.val); err != nil {
			return err
		}
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	cfg, err := loop.Discover(loop.DiscoverOpts{
		ControlRepoFlag: controlRepo,
		LoopDirFlag:     loopDir,
		Cwd:             cwd,
		EnvControlRepo:  os.Getenv("AGENTCHUTE_CONTROL_REPO"),
		EnvLoopDir:      os.Getenv("AGENTCHUTE_LOOP_DIR"),
	})
	if err != nil {
		return err
	}

	if body == "" {
		// Read stdin only when it's piped/redirected; never block waiting on a
		// human typing into an interactive terminal. If stdin is a character
		// device (TTY), send an empty body and let the caller pass --body
		// explicitly if they want content.
		if info, err := os.Stdin.Stat(); err == nil && (info.Mode()&os.ModeCharDevice) == 0 {
			bodyBytes, err := io.ReadAll(os.Stdin)
			if err != nil {
				return fmt.Errorf("read body from stdin: %w", err)
			}
			body = string(bodyBytes)
		}
	}

	now := time.Now().UTC()

	// Update sender's own last_seen if registration exists.
	selfPath := cfg.AgentRegistrationPath(fromID)
	if _, err := os.Stat(selfPath); err == nil {
		if err := loop.UpdateLastSeen(selfPath, now); err != nil {
			return fmt.Errorf("update last_seen for %s: %w", fromID, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat own registration: %w", err)
	}

	content := loop.ComposeMessage(now, fromID, toID, taskField, statusField, replyTo, body)

	// Write to recipient's inbox via atomic temp+rename.
	inboxDir := cfg.AgentInboxDir(toID)
	msg, err := loop.WriteInboxMessage(inboxDir, now, fromID, content)
	if err != nil {
		if os.IsNotExist(err) {
			// This happens if the vendor loop directory exists but the recipient's
			// inbox subdirectory does not.
			return fmt.Errorf("write inbox message: recipient %q is not registered; run agentchute register --as %s first (%w)", toID, toID, err)
		}
		return fmt.Errorf("write inbox message: %w", err)
	}

	// Look up recipient's wake_method/wake_target and poke if pokable (per AGENTCHUTE.md §6.2).
	regPath := cfg.AgentRegistrationPath(toID)
	recipientReg, err := loop.ReadRegistration(regPath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "warning: recipient %q is not registered (missing %s); skipping poke\n", toID, regPath)
		} else {
			fmt.Fprintf(os.Stderr, "warning: cannot read recipient registration (%v); skipping poke\n", err)
		}
	} else if !recipientReg.IsPokable() {
		// Non-pokable recipient (watchdog daemon, headless agent). Per spec, sender skips poke.
		fmt.Fprintf(os.Stderr, "info: recipient %q has no reachable wake_method/wake_target; skipping poke (non-pokable)\n", toID)
	} else if err := loop.PokeWakeTarget(recipientReg.WakeMethod, recipientReg.WakeTarget); err != nil {
		// Non-fatal: message was delivered, the poke just didn't reach.
		fmt.Fprintf(os.Stderr, "warning: wake poke failed (%v); message is still delivered\n", err)
	}

	fmt.Printf("Sent %s\n", msg.Filename)
	fmt.Printf("  from:  %s\n", fromID)
	fmt.Printf("  to:    %s\n", toID)
	fmt.Printf("  path:  %s\n", msg.Path)
	return nil
}

func sendUsage(err error) error {
	return fmt.Errorf(`%w
usage: agentchute send --from <sender> --to <recipient> [--task <text>] [--status <status>] [--reply-to <msg-id>] [--body <text>] [--control-repo <path>] [--loop-dir <path>]

  Ways to provide the body (pick one):
    --body "literal text"             short replies
    < body.md                          multi-line body from a file (preferred in restricted shells)
    cat body.md | agentchute send ...    same stdin path via pipe
    --body "$(cat body.md)"            normal shells only; blocked by some sandboxes`, err)
}

func rejectFrontmatterInjection(name, val string) error {
	if strings.ContainsAny(val, "\n\r") {
		return fmt.Errorf("%s: newlines are not allowed", name)
	}
	if strings.TrimSpace(val) == "---" {
		return fmt.Errorf("%s: frontmatter delimiter %q is not allowed", name, "---")
	}
	return nil
}
