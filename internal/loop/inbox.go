package loop

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Message is a parsed inbox or archive message file. The 4-character hex
// collision-resistance nonce embedded in the filename is recoverable via
// ParseInboxFilename if a consumer needs it; we no longer carry it on the
// struct because no caller reads it.
type Message struct {
	Path      string    // absolute path to the file
	Filename  string    // basename (just the file name, no directory)
	Sender    string    // sender agent_id parsed from the filename
	Timestamp time.Time // sender-side timestamp parsed from the filename (UTC, microsecond precision)
}

const (
	// inboxFilenameSuffix is appended to every inbox/archive filename.
	inboxFilenameSuffix = ".md"

	// tempFilePrefix marks an in-progress write. Listed messages skip this prefix.
	tempFilePrefix = ".tmp_"
)

var removeFile = os.Remove

// agentIDPattern matches the agent_id slug rules: lowercase, digits, hyphen.
// Mirrors AGENTCHUTE.md §5: "Lowercase, hyphen-separated, no spaces."
var agentIDPattern = `[a-z0-9][a-z0-9-]*`

// inboxFilenameRE parses inbox filenames per AGENTCHUTE.md §6.1 (the
// filesystem reference encoding of the §6.1 identity tuple).
//
// Format: `<utc-microsecond-timestamp>_from-<sender>_msg-<nonce>.md`
// Timestamp: `YYYY-MM-DDTHH-MM-SS-uuuuuuZ` (`-` instead of `:` for fs portability;
// microseconds zero-padded to 6 digits).
// Nonce: 4 lowercase hex characters.
var inboxFilenameRE = regexp.MustCompile(
	`^(\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2}-\d{6}Z)_from-(` + agentIDPattern + `)_msg-([0-9a-f]{4})\.md$`,
)

// inboxFilenameShapeRE matches filenames that have the structural shape of a
// §6.1 inbox filename (timestamp segment + `_from-<slug>_msg-` nonce segment +
// `.md` suffix) but is permissive on the timestamp and nonce *content*. Per
// AGENTCHUTE.md §11.1, sender inference is allowed when the timestamp or nonce
// is malformed — but NOT when the structural markers themselves are missing.
// A name without the `_from-`, `_msg-`, or `.md` markers is too broken to
// reliably attribute to any sender and would risk routing corrective notices
// to a mis-inferred peer.
var inboxFilenameShapeRE = regexp.MustCompile(
	`^[^/]+_from-(` + agentIDPattern + `)_msg-[^/]+\.md$`,
)

// InferSenderFromFilename returns the sender slug captured from a §6.1-shaped
// name, OR from a filename whose structural markers (`_from-<slug>_msg-`,
// `.md`) are intact but whose timestamp or nonce is malformed. Per
// AGENTCHUTE.md §11.1, inference is intentionally limited to the
// timestamp/nonce-malformed shape: names missing the `_from-`, `_msg-`, or
// `.md` markers are dropped without inference. The returned slug MUST pass
// ValidateAgentID; otherwise ok=false (inferred ids must be valid).
func InferSenderFromFilename(filename string) (string, bool) {
	if m := inboxFilenameRE.FindStringSubmatch(filename); m != nil {
		return m[2], true
	}
	m := inboxFilenameShapeRE.FindStringSubmatch(filename)
	if m == nil {
		return "", false
	}
	if err := ValidateAgentID(m[1]); err != nil {
		return "", false
	}
	return m[1], true
}

// frontmatterFromRE captures the `from:` scalar inside an already-isolated
// frontmatter block. Tolerant of optional surrounding quotes; rejects
// multi-line scalars.
var frontmatterFromRE = regexp.MustCompile(`(?m)^from:[[:space:]]+"?([A-Za-z0-9_.-]+)"?[[:space:]]*$`)

// InferSenderFromFrontmatter best-effort reads path and extracts the `from:`
// scalar ONLY from the first YAML frontmatter block (between the leading
// `---` and the next `---` line). Body-level lines containing `from:` are
// ignored — they're text, not protocol fields. Returns ok=false if the file
// can't be opened, no frontmatter block exists, no `from:` is present in the
// block, or the value doesn't pass ValidateAgentID.
func InferSenderFromFrontmatter(path string) (string, bool) {
	data, err := ReadFileLimit(path, MaxInboxMessageBytes)
	if err != nil {
		return "", false
	}
	block, ok := firstFrontmatterBlock(data)
	if !ok {
		return "", false
	}
	m := frontmatterFromRE.FindStringSubmatch(block)
	if m == nil {
		return "", false
	}
	if err := ValidateAgentID(m[1]); err != nil {
		return "", false
	}
	return m[1], true
}

// firstFrontmatterBlock returns the content between the file's leading `---`
// line and the next `---` line, exclusive of the delimiters. Returns ok=false
// if the file does not start with `---` or no closing delimiter is found.
func firstFrontmatterBlock(content []byte) (string, bool) {
	text := strings.ReplaceAll(string(content), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return "", false
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			return strings.Join(lines[1:i], "\n"), true
		}
	}
	return "", false
}

// QuarantineInboxFile moves srcPath into malformedDir per AGENTCHUTE.md §11.1,
// with a collision-resistant name `<quarantine-ts>_to-<recipient>_<original>`.
// The destination is created atomically; an existing quarantined file with
// the same name is NOT overwritten (returns os.ErrExist).
func QuarantineInboxFile(srcPath, malformedDir, recipient string, now time.Time) (string, error) {
	if err := ValidateAgentID(recipient); err != nil {
		return "", fmt.Errorf("recipient: %w", err)
	}
	if err := ensurePrivateDir(malformedDir); err != nil {
		return "", err
	}
	base := filepath.Base(srcPath)
	quarantineTS := now.UTC().Format("2006-01-02T15-04-05Z")
	destName := fmt.Sprintf("%s_to-%s_%s", quarantineTS, recipient, base)
	destPath := filepath.Join(malformedDir, destName)
	if err := linkNoClobber(srcPath, destPath); err != nil {
		return "", fmt.Errorf("quarantine %s -> %s: %w", srcPath, destPath, err)
	}
	if err := os.Remove(srcPath); err != nil {
		return "", fmt.Errorf("remove source %s after quarantine: %w", srcPath, err)
	}
	if err := syncDir(filepath.Dir(srcPath)); err != nil {
		return "", err
	}
	if err := syncDir(malformedDir); err != nil {
		return "", err
	}
	return destPath, nil
}

// ParseInboxFilename extracts (timestamp, sender, nonce) from an inbox filename.
// Returns an error if the basename does not match the §6.1 reference encoding.
func ParseInboxFilename(filename string) (time.Time, string, string, error) {
	m := inboxFilenameRE.FindStringSubmatch(filename)
	if m == nil {
		return time.Time{}, "", "", fmt.Errorf("filename %q does not match inbox format", filename)
	}
	t, err := parseFilenameTimestamp(m[1])
	if err != nil {
		return time.Time{}, "", "", fmt.Errorf("filename %q: %w", filename, err)
	}
	return t, m[2], m[3], nil
}

// WriteInboxMessage writes content into the recipient's inbox via temp file +
// atomic rename. Returns the resulting Message (with absolute path, parsed
// fields). If the generated filename collides with an existing file (extremely
// unlikely with microsecond + nonce), the function retries with a fresh nonce
// up to 8 times before giving up.
func WriteInboxMessage(inboxDir string, now time.Time, sender string, content []byte) (Message, error) {
	if err := ValidateAgentID(sender); err != nil {
		return Message{}, err
	}

	// Inbox directory must already exist (created during registration).
	if !dirExists(inboxDir) {
		return Message{}, os.ErrNotExist
	}

	const maxAttempts = 8
	for i := 0; i < maxAttempts; i++ {
		nonce, err := generateNonce()
		if err != nil {
			return Message{}, err
		}
		filename := formatInboxFilename(now, sender, nonce)
		finalPath := filepath.Join(inboxDir, filename)

		// A UNIQUE temp per attempt (os.CreateTemp picks an unused name) — not
		// the deterministic .tmp_<final>. Two concurrent same-sender writes that
		// collide on (timestamp, nonce) would otherwise share one temp path: a
		// late writer could clobber an earlier writer's body before it
		// hard-linked the inode, so the link winner would deliver the WRONG
		// body. A unique inode per attempt makes each body independent; the
		// link-no-clobber to finalPath still resolves the final-filename race
		// (loser retries with a fresh nonce AND a fresh temp).
		tempFile, err := os.CreateTemp(inboxDir, tempFilePrefix+"*")
		if err != nil {
			return Message{}, err
		}
		tempPath := tempFile.Name()
		if err := writeAndSyncOpenFile(tempFile, content); err != nil {
			_ = os.Remove(tempPath)
			return Message{}, err
		}

		if err := linkNoClobber(tempPath, finalPath); err != nil {
			_ = os.Remove(tempPath)
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return Message{}, fmt.Errorf("link to %s: %w", finalPath, err)
		}
		if err := removeFile(tempPath); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to remove temp inbox file %s: %v\n", tempPath, err)
		}
		if err := syncDir(inboxDir); err != nil {
			return Message{}, err
		}

		t, _, _, err := ParseInboxFilename(filename)
		if err != nil {
			return Message{}, err
		}
		return Message{
			Path:      finalPath,
			Filename:  filename,
			Sender:    sender,
			Timestamp: t,
		}, nil
	}
	return Message{}, fmt.Errorf("failed to find a non-colliding nonce after %d attempts", maxAttempts)
}

// ListInboxMessages returns inbox messages for a recipient, sorted oldest-first
// by filename (which is timestamp-prefixed). Skips temp files (.tmp_ prefix),
// dotfiles (e.g., .DS_Store), and any file whose name does not parse per
// §6.1. Returns an empty slice if the inbox dir is missing or empty.
//
// Use ListInboxMessagesWithSkipped when callers need to surface filenames that
// look like message attempts but failed parsing (e.g., to warn an operator
// about a peer that's hand-writing files with malformed nonces).
func ListInboxMessages(inboxDir string) ([]Message, error) {
	msgs, _, err := ListInboxMessagesWithSkipped(inboxDir)
	return msgs, err
}

// ErrInboxMissing is returned by ListInboxMessages* when the recipient's
// inbox directory does not exist on disk. Callers that act AS the agent
// (check, send, watch, gate, self-poll) should treat this as "not
// enrolled" — i.e., the agent never ran boot. Callers operating on a
// peer's inbox (watchdog, status overview, peer liveness) should map it
// to "no mail observable" and continue without failing the whole pass.
//
// Wrap with errors.Is to detect:
//
//	if errors.Is(err, loop.ErrInboxMissing) { ... }
//
// (Introduced in v0.2.1 — "Enforced Enrollment". Replaces the previous
// silent `(nil, nil, nil)` return that masked missing-registration setups
// as empty inboxes.)
var ErrInboxMissing = errors.New("agentchute: inbox directory missing")

// ListInboxMessagesWithSkipped returns both the parsed messages and the
// filenames that look like a message attempt but failed §6.1 parsing. Skipped
// names exclude expected noise (directories, symlinks, dotfiles, .tmp_ files);
// they include only regular files that operator-visible loop activity would
// have written. Skipped names are sorted for deterministic output.
//
// Returns a wrapped ErrInboxMissing when inboxDir does not exist; use
// errors.Is to detect it.
func ListInboxMessagesWithSkipped(inboxDir string) ([]Message, []string, error) {
	entries, err := os.ReadDir(inboxDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, fmt.Errorf("%w: %s", ErrInboxMissing, inboxDir)
		}
		return nil, nil, err
	}

	var msgs []Message
	var skipped []string
	for _, entry := range entries {
		regular, err := isRegularDirEntry(entry)
		if err != nil {
			return nil, nil, err
		}
		if !regular {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, tempFilePrefix) {
			continue
		}
		if strings.HasPrefix(name, ".") {
			continue
		}
		t, sender, _, err := ParseInboxFilename(name)
		if err != nil {
			// Looks like a message but doesn't parse: record so cmdCheck can
			// surface it. Hand-written files with invalid nonces / timestamps
			// land here and would otherwise be silently invisible.
			skipped = append(skipped, name)
			continue
		}
		abs, err := filepath.Abs(filepath.Join(inboxDir, name))
		if err != nil {
			return nil, nil, err
		}
		msgs = append(msgs, Message{
			Path:      abs,
			Filename:  name,
			Sender:    sender,
			Timestamp: t,
		})
	}

	// Filename is timestamp-prefixed in lexicographic-friendly form.
	sort.Slice(msgs, func(i, j int) bool { return msgs[i].Filename < msgs[j].Filename })
	sort.Strings(skipped)
	return msgs, skipped, nil
}

func isRegularDirEntry(entry os.DirEntry) (bool, error) {
	info, err := entry.Info()
	if err != nil {
		return false, err
	}
	mode := info.Mode()
	return mode&os.ModeSymlink == 0 && mode.IsRegular(), nil
}

// ArchiveMessage moves a consumed message to archiveDir using the spec'd
// archive filename: `<consumed-timestamp>_to-<recipient>_<original-inbox-filename>`.
// Returns the absolute archive path so callers (e.g., the v0.1.1 ledger
// integration in `check`) can record traceability without recomputing the
// filename format.
//
// `consumedAt` is the time the recipient consumed the message; it is formatted
// with second precision (suffixed `Z`) for archive sorting and human readability.
//
// The move is atomic via os.Rename when source and destination share a
// filesystem (the normal case for in-repo state).
// ArchiveMessageDest returns the absolute archive path ArchiveMessage will
// write for the given message and consumedAt time, WITHOUT touching the
// filesystem. It is deterministic in (consumedAt, recipient, msg.Filename), so
// `check` can record the pending-reply obligation with the correct archive_path
// BEFORE the message is moved out of the inbox (record-before-archive). The
// returned path stays valid as long as ArchiveMessage is called with the same
// arguments.
func ArchiveMessageDest(msg Message, archiveDir, recipient string, consumedAt time.Time) string {
	archivedName := fmt.Sprintf("%s_to-%s_%s", formatArchiveTimestamp(consumedAt), recipient, msg.Filename)
	return filepath.Join(archiveDir, archivedName)
}

func ArchiveMessage(msg Message, archiveDir, recipient string, consumedAt time.Time) (string, error) {
	if err := ValidateAgentID(recipient); err != nil {
		return "", fmt.Errorf("recipient: %w", err)
	}
	if msg.Path == "" || msg.Filename == "" {
		return "", fmt.Errorf("ArchiveMessage: message Path and Filename required")
	}
	if err := ensurePrivateDir(archiveDir); err != nil {
		return "", err
	}
	dest := ArchiveMessageDest(msg, archiveDir, recipient, consumedAt)
	if err := linkNoClobber(msg.Path, dest); err != nil {
		if errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("archive destination %s already exists", dest)
		}
		return "", fmt.Errorf("archive %s -> %s: %w", msg.Path, dest, err)
	}
	if err := os.Remove(msg.Path); err != nil {
		return "", fmt.Errorf("remove archived source %s: %w", msg.Path, err)
	}
	if err := syncDir(filepath.Dir(msg.Path)); err != nil {
		return "", err
	}
	if err := syncDir(archiveDir); err != nil {
		return "", err
	}
	return dest, nil
}

// writeAndSyncOpenFile writes content into an already-open temp file, fixes its
// mode to 0600, fsyncs the data, and closes it. Mirrors atomicWriteFile's
// durability sequence for the case where the caller created the temp via
// os.CreateTemp (to get a unique inode) and will hard-link it into place itself.
// The caller is responsible for removing the temp on any error AND on the
// normal post-link cleanup path.
func writeAndSyncOpenFile(f *os.File, content []byte) error {
	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func linkNoClobber(oldPath, newPath string) error {
	if err := os.Link(oldPath, newPath); err != nil {
		return err
	}
	if err := syncDir(filepath.Dir(newPath)); err != nil {
		_ = os.Remove(newPath)
		return err
	}
	return nil
}

// formatInboxFilename returns the canonical inbox filename for the given
// fields. Caller must have already validated sender and nonce.
func formatInboxFilename(t time.Time, sender, nonce string) string {
	return fmt.Sprintf("%s_from-%s_msg-%s%s", formatFilenameTimestamp(t), sender, nonce, inboxFilenameSuffix)
}

// formatFilenameTimestamp produces `YYYY-MM-DDTHH-MM-SS-uuuuuuZ` (microseconds,
// zero-padded). Designed for filename portability across macOS/Linux/Windows
// (no `:` characters) while preserving lexicographic time ordering.
func formatFilenameTimestamp(t time.Time) string {
	t = t.UTC()
	micro := t.Nanosecond() / 1000
	return fmt.Sprintf("%04d-%02d-%02dT%02d-%02d-%02d-%06dZ",
		t.Year(), int(t.Month()), t.Day(),
		t.Hour(), t.Minute(), t.Second(),
		micro)
}

// parseFilenameTimestamp inverts formatFilenameTimestamp.
func parseFilenameTimestamp(s string) (time.Time, error) {
	// Layout: 0123456789012345678901234567
	//         2026-05-09T16-32-00-123456Z
	if len(s) != 27 || s[26] != 'Z' {
		return time.Time{}, fmt.Errorf("expected YYYY-MM-DDTHH-MM-SS-uuuuuuZ, got %q", s)
	}
	parts := []struct {
		start, end int
	}{
		{0, 4},   // year
		{5, 7},   // month
		{8, 10},  // day
		{11, 13}, // hour
		{14, 16}, // minute
		{17, 19}, // second
		{20, 26}, // microsecond
	}
	nums := make([]int, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(s[p.start:p.end])
		if err != nil {
			return time.Time{}, fmt.Errorf("parse component %d-%d of %q: %w", p.start, p.end, s, err)
		}
		nums[i] = n
	}
	year, month, day, hour, minute, sec, micro := nums[0], nums[1], nums[2], nums[3], nums[4], nums[5], nums[6]
	if month < 1 || month > 12 {
		return time.Time{}, fmt.Errorf("invalid month %d in %q", month, s)
	}
	t := time.Date(year, time.Month(month), day, hour, minute, sec, micro*1000, time.UTC)
	// time.Date normalizes invalid dates (e.g., Feb 31 → Mar 3). Reject those by
	// confirming every parsed component round-trips through the constructed time.
	if t.Year() != year || int(t.Month()) != month || t.Day() != day ||
		t.Hour() != hour || t.Minute() != minute || t.Second() != sec ||
		t.Nanosecond()/1000 != micro {
		return time.Time{}, fmt.Errorf("invalid calendar date in %q", s)
	}
	return t, nil
}

// formatArchiveTimestamp returns `YYYY-MM-DDTHH-MM-SSZ` (second precision, no
// microseconds — archive timestamps just need to sort and be human-readable;
// the original inbox filename in the archive name preserves microsecond identity).
func formatArchiveTimestamp(t time.Time) string {
	t = t.UTC()
	return fmt.Sprintf("%04d-%02d-%02dT%02d-%02d-%02dZ",
		t.Year(), int(t.Month()), t.Day(),
		t.Hour(), t.Minute(), t.Second())
}

// generateNonce is a package var (not a plain func) only so tests can pin it to
// force the filename/temp-path collision retry path deterministically.
var generateNonce = func() (string, error) {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// ValidateAgentID enforces the agent_id slug rules from AGENTCHUTE.md §5
// (lowercase, digits, hyphen, must start with alnum). The regex already
// rejects path separators, dot segments, and other path-traversal vectors,
// so callers SHOULD invoke this on every user-controlled agent_id-shaped
// flag (--as, --from, --to) before any filesystem resolution.
var agentIDRE = regexp.MustCompile(`^` + agentIDPattern + `$`)

func ValidateAgentID(id string) error {
	if id == "" {
		return fmt.Errorf("agent_id is required")
	}
	if !agentIDRE.MatchString(id) {
		return fmt.Errorf("agent_id %q must match %s", id, agentIDPattern)
	}
	return nil
}
