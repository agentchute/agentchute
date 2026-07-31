package loop

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ComposeMessage builds an outbound message's bytes (frontmatter + body)
// per AGENTCHUTE.md §6.4. Body is markdown; a trailing newline is normalized
// regardless of the input.
//
// protocol-v2 envelope (TEAM-DECISION §4, P1 residue cleanup): the envelope is
// `from` plus an optional `in_reply_to`. `to` is encoded by the inbox
// directory, not emitted; a message's subject, if any, is a body convention
// (first Markdown line), not a typed field. `now`/`to`/`task`/`status` were
// compat-only parameters (unused; the envelope stopped emitting them in
// v0.9.0) and are gone from the signature entirely, not just unemitted.
func ComposeMessage(from, replyTo, body string) []byte {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "from: %s\n", from)
	if replyTo != "" {
		fmt.Fprintf(&b, "in_reply_to: %s\n", quoteIfNeeded(replyTo))
	}
	b.WriteString("---\n\n")
	b.WriteString(strings.TrimRight(body, "\n"))
	b.WriteString("\n")
	return []byte(b.String())
}

// AnnounceResult is the outcome of AnnounceEnrollment: how many peers were
// candidates, how many got the message in their inbox, and any per-peer
// delivery (inbox-write) warnings. A non-empty Warnings list is normal
// and not fatal; register reports them to stderr and exits 0 regardless.
type AnnounceResult struct {
	Total    int
	Sent     int
	Warnings []string
}

// AnnounceEnrollment sends a directly-addressed enrollment notification to
// every currently-registered peer in cfg's agents dir (skipping self, the
// tracked *.example.md files, dotfiles, and non-.md entries). It is N direct
// sends — NOT a broadcast mechanism — and stays within AGENTCHUTE.md §7.1.
//
// Per-peer failures (missing inbox, malformed registration, a stale peer per
// B3's freshness enforcement) are collected as Warnings; the function does
// not abort on them. A returned error means the agents directory itself
// could not be read. Delivery goes through the same locked path send.go
// uses (SendSeqMessage -> DeliverUnderRecipientLock): a stale peer is simply
// skipped with a warning, exactly like any other per-peer failure — there is
// no separate preflight here (AnnounceEnrollment has no user-facing C29
// wording to choose between; every freshness failure reads the same way).
func AnnounceEnrollment(cfg *Config, self *Registration) (AnnounceResult, error) {
	entries, err := os.ReadDir(cfg.AgentsDir())
	if err != nil {
		return AnnounceResult{}, fmt.Errorf("read agents dir: %w", err)
	}
	var result AnnounceResult
	body := announcementBody(self)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		if !strings.HasSuffix(name, ".md") || strings.HasSuffix(name, ".example.md") {
			continue
		}
		if name == "README.md" {
			continue
		}
		peerPath := filepath.Join(cfg.AgentsDir(), name)
		peer, err := ReadRegistration(peerPath)
		if err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		if peer.AgentID == self.AgentID {
			continue
		}
		result.Total++
		content := ComposeMessage(self.AgentID, "", body)
		// Deliver under the new timestamp identity (v2.5 plan B7): mint a
		// stamp under self's own lock, release, then deliver under peer's
		// lock (C8). Empty serveToken means intentionally unfenced — an
		// enrollment announcement is not gated on a live serve lease.
		if _, _, err := SendTsMessageWithCommit(cfg, self.AgentID, peer.AgentID, content, ""); err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("send to %s: %v", peer.AgentID, err))
			continue
		}
		result.Sent++
		// Simple-again Gate 6a (pull-only): the announcement is delivered by the
		// inbox file write alone; peers pick it up on their own poll. No wake poke.
	}
	return result, nil
}

// ValidateMessageFrontmatter applies the §11.1 frontmatter trigger: if the
// content has an opening `---` line but no closing `---` or the block between
// them cannot be parsed as key:value YAML, return an error describing the
// failure. Returns nil for body-only messages (no leading `---`; §6.4 says
// frontmatter is recommended, not required).
func ValidateMessageFrontmatter(content []byte) error {
	text := strings.ReplaceAll(string(content), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return nil // body-only is valid per §6.4
	}
	_, _, err := parseFrontmatter(text)
	return err
}

// ExtractMessageBody returns the body portion of a message (everything
// after the closing frontmatter `---` line). Body-splitting only depends on
// where the delimiters are, never on whether the key:value content between
// them parses, so this shares frontmatterClosingLine (registration.go) with
// parseFrontmatter rather than validating the block itself (v2.5 plan B8).
// Returns the full content unchanged when there's no frontmatter block
// (body-only is valid per §6.4) or when the open delimiter has no
// matching close.
func ExtractMessageBody(content []byte) string {
	text := strings.ReplaceAll(string(content), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return text
	}
	closing := frontmatterClosingLine(lines)
	if closing == -1 {
		return text
	}
	// Body starts on the line after the closing delimiter; a blank line
	// immediately following is conventional but not required, so don't trim it.
	return strings.Join(lines[closing+1:], "\n")
}

// ParseMessageFrontmatter extracts the leading frontmatter block from a
// message's bytes into a flat key/value map, using the SAME strict engine
// (parseFrontmatter, registration.go) as ValidateMessageFrontmatter — the two
// can no longer disagree on what counts as a well-formed block (v2.5 plan
// B8; this closes the validator/recorder skew WI-10 named). Body-only
// messages return an empty map. Malformed blocks (opening `---` with no
// close, an indented line, a non-key:value line, a duplicate key, an empty
// key) also return an empty map; callers that need malformed-vs-absent
// distinction should call ValidateMessageFrontmatter first. List-shaped
// fields (e.g. working_repos) surface with an empty string, matching
// parseFrontmatter's own scalar/list split — no current message field is
// list-valued.
func ParseMessageFrontmatter(content []byte) map[string]string {
	out := map[string]string{}
	text := strings.ReplaceAll(string(content), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return out
	}
	fields, _, err := parseFrontmatter(text)
	if err != nil {
		return out
	}
	for key := range fields {
		out[key] = fields.scalar(key)
	}
	return out
}

// announcementBody is the human- and machine-readable payload for an
// enrollment notification. Declarative-neutral; no salutation. Renders well
// in `agentchute check` output and is also parseable by another agent.
func announcementBody(self *Registration) string {
	bio := strings.TrimSpace(self.Body)
	if bio == "" {
		bio = "(no bio set; see agentchute status or this agent's registration body)"
	}
	return fmt.Sprintf("Agent registration: %s (%s)\n\n%s", self.AgentID, self.Vendor, bio)
}
