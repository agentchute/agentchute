package conformance

import (
	"fmt"
	"strconv"
	"strings"
)

// fm.go — v2.5 plan B8: an independent reimplementation of the ONE flat
// key:value frontmatter grammar (AGENTCHUTE.md §6.4/§5.2), mirroring but not
// importing internal/loop's parseFrontmatter (tsid.go's own posture: prove
// the WIRE GRAMMAR, not just the reference CLI's Go code). Any binding's
// envelope parser must satisfy the FM1 (accept) / FM2 (reject) vectors this
// file is exercised against.

// ParseFrontmatterFields parses a message's leading frontmatter block into a
// flat key/value map. A body-only input (no leading `---`) is not an error —
// it returns an empty map, nil. A malformed block (an indented line, a
// non-key:value line — including a `#` comment, since this grammar has no
// comment syntax — a duplicate key, an empty key, or a missing closing
// `---`) returns a non-nil error and a nil map; the whole block is rejected,
// not just the offending line.
func ParseFrontmatterFields(content string) (map[string]string, error) {
	text := strings.ReplaceAll(content, "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return map[string]string{}, nil
	}

	closing := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			closing = i
			break
		}
	}
	if closing == -1 {
		return nil, fmt.Errorf("missing frontmatter closing ---")
	}

	fields := map[string]string{}
	for i := 1; i < closing; i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			return nil, fmt.Errorf("unexpected indented line %q", line)
		}

		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("invalid frontmatter line %q", line)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" {
			return nil, fmt.Errorf("empty frontmatter key")
		}
		if _, exists := fields[key]; exists {
			return nil, fmt.Errorf("duplicate frontmatter key %q", key)
		}

		if value != "" {
			fields[key] = cleanFmScalar(value)
			continue
		}

		// A bare "key:" opens a list: consume following "  - item" lines.
		// List items are not surfaced in the flat scalar map (the header key
		// gets an empty string) — matches ParseMessageFrontmatter's own
		// scalar-only view; no current message field is list-valued.
		for i+1 < closing {
			trimmed := strings.TrimSpace(lines[i+1])
			if trimmed == "" {
				i++
				continue
			}
			if strings.HasPrefix(trimmed, "- ") {
				i++
				continue
			}
			break
		}
		fields[key] = ""
	}
	return fields, nil
}

// cleanFmScalar mirrors internal/loop's cleanScalar: strconv.Unquote first
// (interprets backslash escapes), falling back to a single paired-quote
// strip; `null`/`~` collapse to empty string.
func cleanFmScalar(value string) string {
	if value == "null" || value == "~" {
		return ""
	}
	if unquoted, err := strconv.Unquote(value); err == nil {
		return unquoted
	}
	if len(value) >= 2 {
		first, last := value[0], value[len(value)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			return value[1 : len(value)-1]
		}
	}
	return value
}
