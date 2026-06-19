package loop

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PointerFileName is the canonical filename used for cross-folder pointer
// files (AGENTCHUTE.md §4 / §4.2). The pointer file's single non-comment line
// is a path to the agentchute control repo this folder should coordinate
// through.
const PointerFileName = ".agentchute-control-repo"

// Pointer is a parsed + resolved pointer file. ResolvedTarget is the absolute
// path the pointer points at; PointerFilePath is where the pointer file was
// read from; Shadowed lists any ancestor pointer files that were skipped
// because a nearer pointer was found (informational, for diagnostics).
type Pointer struct {
	ResolvedTarget  string
	PointerFilePath string
	Shadowed        []string
}

// ParsePointerFile extracts the target path from a pointer file's contents.
// The grammar is intentionally minimal: blank lines and lines beginning with
// `#` (after optional leading whitespace) are ignored; exactly one remaining
// non-empty line is required, and that line is the path. More than one
// content line is an error.
func ParsePointerFile(content string) (string, error) {
	text := strings.ReplaceAll(content, "\r\n", "\n")
	var found string
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if found != "" {
			return "", fmt.Errorf("%s: more than one non-comment path line", PointerFileName)
		}
		found = trimmed
	}
	if found == "" {
		return "", fmt.Errorf("%s: no path found (file is blank or only comments)", PointerFileName)
	}
	return found, nil
}

// DiscoverPointer walks from startDir up to the filesystem root looking for
// .agentchute-control-repo files. The nearest pointer wins; any others found
// during the walk are returned in Shadowed for diagnostics. Returns
// (nil, nil) if no pointer file exists in the ancestor chain — discovery
// continues to the next cascade step in that case.
func DiscoverPointer(startDir string) (*Pointer, error) {
	dir, err := filepath.Abs(startDir)
	if err != nil {
		return nil, fmt.Errorf("pointer discovery abs(%q): %w", startDir, err)
	}

	var nearest string
	var shadowed []string
	for {
		candidate := filepath.Join(dir, PointerFileName)
		if fileExists(candidate) {
			if nearest == "" {
				nearest = candidate
			} else {
				shadowed = append(shadowed, candidate)
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	if nearest == "" {
		return nil, nil
	}

	raw, err := os.ReadFile(nearest)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", nearest, err)
	}
	target, err := ParsePointerFile(string(raw))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", nearest, err)
	}
	resolved, err := ResolvePointerTarget(filepath.Dir(nearest), target)
	if err != nil {
		return nil, fmt.Errorf("%s -> %q: %w", nearest, target, err)
	}
	return &Pointer{
		ResolvedTarget:  resolved,
		PointerFilePath: nearest,
		Shadowed:        shadowed,
	}, nil
}

// ResolvePointerTarget normalizes raw against pointerDir, returning an
// absolute path. Relative paths resolve from pointerDir; absolute paths are
// accepted as-is. The resolved target must be an existing directory. Note:
// the resolved path is allowed to leave the pointer file's containing repo
// — sibling-repo pointers (e.g., `../coordination`) are the primary use
// case the design enables. Target *validity* (the directory contains
// AGENTCHUTE.md and a vendor loop dir) is enforced separately by callers
// that invoke discoverControlRepo.
func ResolvePointerTarget(pointerDir, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("pointer target is empty")
	}
	if !filepath.IsAbs(raw) {
		raw = filepath.Join(pointerDir, raw)
	}
	return absExistingDir(raw)
}
