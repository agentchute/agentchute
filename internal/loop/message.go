package loop

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ComposeMessage builds an outbound message's bytes (frontmatter + body)
// per AGENTCHUTE.md §6.4. Optional scalars (task, status, replyTo) may be
// empty. Body is markdown; a trailing newline is normalized regardless of
// the input.
func ComposeMessage(now time.Time, from, to, task, status, replyTo, body string) []byte {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "message_id: %s\n", formatMessageID(now))
	fmt.Fprintf(&b, "from: %s\n", from)
	fmt.Fprintf(&b, "to: %s\n", to)
	if replyTo != "" {
		fmt.Fprintf(&b, "in_reply_to: %s\n", quoteIfNeeded(replyTo))
	}
	if task != "" {
		fmt.Fprintf(&b, "task: %s\n", quoteIfNeeded(task))
	}
	if status != "" {
		fmt.Fprintf(&b, "status: %s\n", quoteIfNeeded(status))
	}
	b.WriteString("---\n\n")
	b.WriteString(strings.TrimRight(body, "\n"))
	b.WriteString("\n")
	return []byte(b.String())
}

// formatMessageID returns the recommended frontmatter `message_id` format
// (RFC 3339 UTC with microsecond precision and `:` separators preserved).
// Distinct from filename timestamps which use `-` for filesystem portability.
func formatMessageID(t time.Time) string {
	t = t.UTC()
	return fmt.Sprintf("%04d-%02d-%02dT%02d:%02d:%02d.%06dZ",
		t.Year(), int(t.Month()), t.Day(),
		t.Hour(), t.Minute(), t.Second(),
		t.Nanosecond()/1000)
}

// AnnounceResult is the outcome of AnnounceEnrollment: how many peers were
// candidates, how many got the message in their inbox, and any per-peer
// warnings (delivery or poke failures). A non-empty Warnings list is normal
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
// Per-peer failures (missing inbox, failed wake poke, malformed registration)
// are collected as Warnings; the function does not abort on them. A returned
// error means the agents directory itself could not be read.
func AnnounceEnrollment(cfg *Config, self *Registration, now time.Time) (AnnounceResult, error) {
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
		content := ComposeMessage(now, self.AgentID, peer.AgentID, "enrolled", "info", "", body)
		if _, err := WriteInboxMessage(cfg.AgentInboxDir(peer.AgentID), now, self.AgentID, content); err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("send to %s: %v", peer.AgentID, err))
			continue
		}
		result.Sent++
		if peer.IsPokable() {
			if err := PokeWakeTarget(peer.WakeMethod, peer.WakeTarget); err != nil {
				result.Warnings = append(result.Warnings, fmt.Sprintf("poke %s: %v", peer.AgentID, err))
			}
		}
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

// CorrectiveBody renders the §11.3 protocol-correction body for a quarantined
// item with a specific reason and section reference. Three lines, compiler-
// error shape, no conversational framing.
func CorrectiveBody(malformedItem, reason, sectionRef string) string {
	return fmt.Sprintf("malformed item: %s\nreason: %s\naction: re-send per AGENTCHUTE.md %s\n",
		malformedItem, reason, sectionRef)
}

// SendCorrective is the §11 enforcement send: composes a "protocol correction"
// message and writes it to the offender's inbox, then pokes their tmux pane if
// they have one. Returns the resulting Message on success.
//
// Per §11.4: best-effort. If the offender's registration is unreadable or has
// no reachable wake method, the message still lands in the inbox; the poke is
// skipped. If the offender's inbox dir doesn't exist, the corrective send
// fails — the caller leaves the file quarantined and logs locally without
// retrying.
func SendCorrective(cfg *Config, from, offender, malformedItem, reason, sectionRef string, now time.Time) (Message, error) {
	body := CorrectiveBody(malformedItem, reason, sectionRef)
	content := ComposeMessage(now, from, offender, "protocol correction", "findings", "", body)

	msg, err := WriteInboxMessage(cfg.AgentInboxDir(offender), now, from, content)
	if err != nil {
		return Message{}, err
	}

	// Best-effort poke (per §11.4 / §6.2).
	reg, err := ReadRegistration(cfg.AgentRegistrationPath(offender))
	if err == nil && reg.IsPokable() {
		_ = PokeWakeTarget(reg.WakeMethod, reg.WakeTarget)
	}
	return msg, nil
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
