package loop

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Message is a parsed inbox or archive message file.
//
// Timestamp is populated from the timestamp-format filename when present, or
// from file mtime for legacy seq messages. It is advisory display/staleness
// data only; ordering uses the parsed delivery identity.
type Message struct {
	Path      string    // absolute path to the file
	Filename  string    // basename (just the file name, no directory)
	Sender    string    // sender agent_id parsed from the filename
	Timestamp time.Time // embedded stamp or legacy mtime; advisory only
}

const (
	// inboxFilenameSuffix is appended to every inbox/archive filename.
	inboxFilenameSuffix = ".md"

	// tempFilePrefix marks an in-progress write. Listed messages skip this prefix.
	tempFilePrefix = ".tmp_"
)

var (
	readInboxDir = os.ReadDir
)

// agentIDPattern matches the agent_id slug rules: lowercase, digits, hyphen.
// Mirrors AGENTCHUTE.md §5: "Lowercase, hyphen-separated, no spaces."
var agentIDPattern = `[a-z0-9][a-z0-9-]*`

// InferSenderFromFilename returns the sender slug captured from either canonical
// inbox filename grammar.
func InferSenderFromFilename(filename string) (string, bool) {
	if from, _, ok := ParseSeqFilename(filename); ok {
		return from, true
	}
	if id, ok := ParseTsFilename(filename); ok {
		return id.From, true
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

// ListInboxMessages returns inbox messages for a recipient, sorted by sender,
// then legacy-before-timestamp grammar, then each grammar's own key. It skips
// temp files (.tmp_ prefix), dotfiles (e.g., .DS_Store), and names that parse
// as neither supported grammar. A missing or empty inbox returns an empty slice.
//
// Use ListInboxMessagesWithSkipped when callers need to surface filenames that
// look like message attempts but failed parsing (e.g., to warn an operator
// about a peer that's hand-writing files with malformed names).
func ListInboxMessages(inboxDir string) ([]Message, error) {
	msgs, _, err := ListInboxMessagesWithSkipped(inboxDir)
	return msgs, err
}

// ErrInboxMissing is returned by ListInboxMessages* when the recipient's
// inbox directory does not exist on disk. Callers that act AS the agent
// (check, send, gate) should treat this as "not
// enrolled" — i.e., the agent never ran boot. Callers scanning a
// peer's inbox (the status overview, peer enumeration) should map it
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
	entries, err := readInboxDir(inboxDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, fmt.Errorf("%w: %s", ErrInboxMissing, inboxDir)
		}
		return nil, nil, err
	}

	var msgs []Message
	var skipped []string
	for _, entry := range entries {
		regular, modTime, err := isRegularDirEntry(entry)
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
		// A file whose name parses as either canonical grammar is a real message;
		// only a genuinely-unrecognized name is `skipped` (and thus subject to
		// §11 quarantine).
		msg, ok := parseAnyInboxName(name, modTime)
		if !ok {
			skipped = append(skipped, name)
			continue
		}
		abs, err := filepath.Abs(filepath.Join(inboxDir, name))
		if err != nil {
			return nil, nil, err
		}
		msg.Path = abs
		msg.Filename = name
		msgs = append(msgs, msg)
	}

	sortInboxMessages(msgs)
	sort.Strings(skipped)
	return msgs, skipped, nil
}

// sortInboxMessages preserves per-sender FIFO through the one-way migration:
// all legacy messages from a sender precede timestamp-format messages, and each
// grammar is ordered by its own monotonic key. Cross-sender order is advisory.
func sortInboxMessages(msgs []Message) {
	sort.Slice(msgs, func(i, j int) bool {
		a, b := msgs[i], msgs[j]
		if a.Sender != b.Sender {
			return a.Sender < b.Sender
		}

		_, aSeq, aOld := ParseSeqFilename(a.Filename)
		_, bSeq, bOld := ParseSeqFilename(b.Filename)
		if aOld != bOld {
			return aOld
		}
		if aOld {
			if aSeq != bSeq {
				return aSeq < bSeq
			}
			return a.Filename < b.Filename
		}

		aTs, aNew := ParseTsFilename(a.Filename)
		bTs, bNew := ParseTsFilename(b.Filename)
		if aNew && bNew {
			if aTs.Stamp != bTs.Stamp {
				return aTs.Stamp < bTs.Stamp
			}
			if aTs.Suffix != bTs.Suffix {
				return aTs.Suffix < bTs.Suffix
			}
		}
		return a.Filename < b.Filename
	})
}

// isRegularDirEntry reports whether entry is a plain regular file (not a
// symlink/dir/socket) and returns its mtime in the SAME Info() stat — so the
// lister can populate a Message's advisory Timestamp without a second filesystem
// stat. The returned time is the zero value when the entry is not a regular file
// (callers ignore it in that case).
func isRegularDirEntry(entry os.DirEntry) (bool, time.Time, error) {
	info, err := entry.Info()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, time.Time{}, nil
		}
		return false, time.Time{}, err
	}
	mode := info.Mode()
	return mode&os.ModeSymlink == 0 && mode.IsRegular(), info.ModTime(), nil
}

// parseAnyInboxName is the shared dual-read gate for inbox and claimed listers.
// Legacy seq names use mtime as the advisory timestamp; timestamp-format names
// use their embedded timestamp.
func parseAnyInboxName(name string, modTime time.Time) (Message, bool) {
	if from, _, ok := ParseSeqFilename(name); ok {
		return Message{Sender: from, Timestamp: modTime}, true
	}
	if id, ok := ParseTsFilename(name); ok {
		stamp, _ := ParseStamp(id.Stamp)
		return Message{Sender: id.From, Timestamp: stamp}, true
	}
	return Message{}, false
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
// write for the given message and consumedAt time, without touching the
// filesystem. It is deterministic in (consumedAt, recipient, msg.Filename);
// the returned path stays valid as long as ArchiveMessage is called with the
// same arguments.
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
			if ok, removeSource, completeErr := archiveAlreadyComplete(msg.Path, dest); completeErr != nil {
				return "", completeErr
			} else if ok {
				if removeSource {
					if err := os.Remove(msg.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
						return "", fmt.Errorf("remove archived source %s: %w", msg.Path, err)
					}
					if err := syncDir(filepath.Dir(msg.Path)); err != nil {
						return "", err
					}
				}
				if err := syncDir(archiveDir); err != nil {
					return "", err
				}
				return dest, nil
			}
			return "", fmt.Errorf("archive destination %s already exists", dest)
		}
		if errors.Is(err, os.ErrNotExist) {
			if _, statErr := os.Stat(dest); statErr == nil {
				return dest, nil
			}
		}
		return "", fmt.Errorf("archive %s -> %s: %w", msg.Path, dest, err)
	}
	if err := os.Remove(msg.Path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if err := syncDir(archiveDir); err != nil {
				return "", err
			}
			return dest, nil
		}
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

// ClaimMessage is phase 1 of the two-phase consume (Gate 5): it MOVES a message
// out of the inbox into claimedDir under its CANONICAL name (no archive
// timestamp), so the displayed-but-not-yet-committed message survives a crash
// between `check` (claim) and `ack` (commit) — at-least-once for the agent's
// WORK. It mirrors WriteSeqMessage/ArchiveMessage's link-no-clobber + remove
// discipline. Returns the claimed-copy path.
//
// IDEMPOTENT / DEDUP (modeled on ArchiveMessage's EEXIST/SameFile path): the
// canonical delivery name encodes IDENTITY, so if it already sits in claimedDir
// — whether the same inode (a re-claim) or a fresh inode (a resend that re-landed
// the SAME identity in the inbox while the original was already claimed) — the
// inbox source is the SAME protocol message. We DROP it (remove the source) and
// return the already-claimed copy rather than erroring. (This is the one
// divergence from ArchiveMessage, which errors on a different-inode EEXIST; for
// a claim a same-identity resend is a benign duplicate to discard.)
func ClaimMessage(msg Message, claimedDir string) (string, error) {
	if msg.Path == "" || msg.Filename == "" {
		return "", fmt.Errorf("ClaimMessage: message Path and Filename required")
	}
	if err := ensurePrivateDir(claimedDir); err != nil {
		return "", err
	}
	dest := filepath.Join(claimedDir, msg.Filename)
	if err := linkNoClobber(msg.Path, dest); err != nil {
		if errors.Is(err, os.ErrExist) {
			// Same canonical identity already claimed → drop the inbox source
			// (dedup) and keep the claimed copy.
			if _, statErr := os.Stat(msg.Path); statErr != nil {
				if errors.Is(statErr, os.ErrNotExist) {
					return dest, nil // source already gone — fully claimed.
				}
				return "", fmt.Errorf("stat claim source %s: %w", msg.Path, statErr)
			}
			if err := os.Remove(msg.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return "", fmt.Errorf("remove duplicate claim source %s: %w", msg.Path, err)
			}
			if err := syncDir(filepath.Dir(msg.Path)); err != nil {
				return "", err
			}
			return dest, nil
		}
		if errors.Is(err, os.ErrNotExist) {
			// Source vanished mid-claim (a concurrent claimer) but the claimed
			// copy may already be in place.
			if _, statErr := os.Stat(dest); statErr == nil {
				return dest, nil
			}
		}
		return "", fmt.Errorf("claim %s -> %s: %w", msg.Path, dest, err)
	}
	if err := os.Remove(msg.Path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Linked, then a concurrent claimer removed the source. The link
			// succeeded, so the claimed copy is authoritative.
			if err := syncDir(claimedDir); err != nil {
				return "", err
			}
			return dest, nil
		}
		return "", fmt.Errorf("remove claimed source %s: %w", msg.Path, err)
	}
	if err := syncDir(filepath.Dir(msg.Path)); err != nil {
		return "", err
	}
	if err := syncDir(claimedDir); err != nil {
		return "", err
	}
	return dest, nil
}

// ListClaimedMessages returns the uncommitted residue in claimedDir — messages
// CLAIMED by a prior `check` but not yet COMMITTED by `ack` (e.g. the agent
// crashed, or hasn't run ack). It reuses parseAnyInboxName and the same
// identity-aware sort as the inbox lister. A missing claimedDir is not an error
// (no residue → empty slice). Unrecognized residue is skipped silently.
func ListClaimedMessages(claimedDir string) ([]Message, error) {
	entries, err := readInboxDir(claimedDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var msgs []Message
	for _, entry := range entries {
		regular, modTime, err := isRegularDirEntry(entry)
		if err != nil {
			return nil, err
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
		msg, ok := parseAnyInboxName(name, modTime)
		if !ok {
			continue
		}
		abs, err := filepath.Abs(filepath.Join(claimedDir, name))
		if err != nil {
			return nil, err
		}
		msg.Path = abs
		msg.Filename = name
		msgs = append(msgs, msg)
	}
	sortInboxMessages(msgs)
	return msgs, nil
}

func archiveAlreadyComplete(src, dest string) (ok, removeSource bool, err error) {
	destInfo, destErr := os.Stat(dest)
	if destErr != nil {
		return false, false, nil
	}
	srcInfo, srcErr := os.Stat(src)
	if srcErr != nil {
		if errors.Is(srcErr, os.ErrNotExist) {
			return true, false, nil
		}
		return false, false, fmt.Errorf("stat archived source %s: %w", src, srcErr)
	}
	if os.SameFile(srcInfo, destInfo) {
		return true, true, nil
	}
	return false, false, nil
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

// formatArchiveTimestamp returns `YYYY-MM-DDTHH-MM-SSZ` (second precision, no
// microseconds — archive timestamps just need to sort and be human-readable;
// the original inbox filename in the archive name preserves microsecond identity).
func formatArchiveTimestamp(t time.Time) string {
	t = t.UTC()
	return fmt.Sprintf("%04d-%02d-%02dT%02d-%02d-%02dZ",
		t.Year(), int(t.Month()), t.Day(),
		t.Hour(), t.Minute(), t.Second())
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
