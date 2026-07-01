package loop

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ComposeMessage builds an outbound message's bytes (frontmatter + body)
// per AGENTCHUTE.md §6.4. Body is markdown; a trailing newline is normalized
// regardless of the input.
//
// protocol-v2 envelope cut (TEAM-DECISION §4): the emitted envelope is now
// `from` plus an optional `in_reply_to`. `to` is dropped — the inbox directory
// location encodes the recipient — and the `task`/`status` workflow-vocabulary fields move to a
// body convention, so neither is emitted. The `to`/`task`/`status` parameters
// are retained on the signature for one release so callers (send, announce,
// corrective) need no change during the transition; the inbox parser still
// READS all of these from any older message in flight.
func ComposeMessage(now time.Time, from, to, task, status, replyTo, body string) []byte {
	_ = now    // no longer seeds a message_id (dropped v0.9.0); param kept for signature stability.
	_ = to     // recipient is encoded by the inbox directory; not emitted (compat param).
	_ = task   // workflow vocabulary → body convention; not emitted (compat param).
	_ = status // workflow vocabulary → body convention; not emitted (compat param).
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
// Per-peer failures (missing inbox, malformed registration) are collected as
// Warnings; the function does not abort on them. A returned error means the
// agents directory itself could not be read.
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
		// Gate 4: deliver under the canonical (to,from,seq) identity (empty
		// idempotencyKey/serveToken = transitional at-most-once, unfenced).
		if _, err := SendSeqMessage(cfg, self.AgentID, peer.AgentID, content, "", ""); err != nil {
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
// after the closing frontmatter `---` line). Honors the same lenient
// delimiter semantics as ParseMessageFrontmatter (trimmed `---` opens
// and closes the block — surrounding whitespace tolerated per §6.4).
// Returns the full content unchanged when there's no frontmatter block
// (body-only is valid per §6.4) or when the open delimiter has no
// matching close.
func ExtractMessageBody(content []byte) string {
	text := strings.ReplaceAll(string(content), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return text
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			// Body starts on the line after the closing delimiter; a
			// blank line immediately following is conventional but not
			// required, so don't trim it.
			return strings.Join(lines[i+1:], "\n")
		}
	}
	return text
}

// ParseMessageFrontmatter extracts the leading frontmatter block from a
// message's bytes into a flat key/value map, honoring the same lenient
// delimiter semantics as ValidateMessageFrontmatter (a trimmed `---` line
// opens the block, a later trimmed `---` line closes it — surrounding
// whitespace tolerated per §6.4). Body-only messages return an empty
// map. Malformed blocks (opening `---` with no close) also return an empty
// map; callers that need malformed-vs-absent distinction should call
// ValidateMessageFrontmatter first.
//
// Mirrors the in-package parser used by `pending` / `check` so the
// hot-path peek (pending.readFrontmatter) and the consume path
// (check.displayConsumed) cannot disagree on what counts as a
// well-formed frontmatter block.
func ParseMessageFrontmatter(content []byte) map[string]string {
	out := map[string]string{}
	block, ok := firstFrontmatterBlock(content)
	if !ok {
		return out
	}
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		key := strings.TrimSpace(line[:colon])
		val := strings.TrimSpace(line[colon+1:])
		val = strings.Trim(val, `"'`)
		out[key] = val
	}
	return out
}

// CorrectiveBody renders the §11.1 protocol-correction body for a quarantined
// item with a specific reason and section reference. Three lines, compiler-
// error shape, no conversational framing.
func CorrectiveBody(malformedItem, reason, sectionRef string) string {
	return fmt.Sprintf("malformed item: %s\nreason: %s\naction: re-send per AGENTCHUTE.md %s\n",
		malformedItem, reason, sectionRef)
}

// SendCorrective is the §11 enforcement send: composes a "protocol correction"
// message and writes it to the offender's inbox. Returns the resulting Message
// on success.
//
// Per §11.1: pull-only delivery. The corrective lands in the offender's inbox
// and the offender picks it up on its own poll — there is no poke. If the
// offender's inbox dir doesn't exist, the corrective send fails — the caller
// leaves the file quarantined and logs locally without retrying.
func SendCorrective(cfg *Config, from, offender, malformedItem, reason, sectionRef string, now time.Time) (Message, error) {
	body := CorrectiveBody(malformedItem, reason, sectionRef)
	content := ComposeMessage(now, from, offender, "protocol correction", "findings", "", body)

	// Gate 4: deliver under the canonical (to,from,seq) identity (empty
	// idempotencyKey/serveToken = transitional at-most-once, unfenced).
	id, err := SendSeqMessage(cfg, from, offender, content, "", "")
	if err != nil {
		return Message{}, err
	}
	msg := Message{
		Filename: id.Filename(),
		Path:     filepath.Join(cfg.AgentInboxDir(offender), id.Filename()),
		Sender:   from,
	}

	// Simple-again Gate 6a (pull-only): the corrective is delivered by the inbox
	// file write alone; the offender picks it up on its own poll. No wake poke.
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
