package loop

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Size caps for agentchute-managed reads. Defense against a buggy or hostile
// peer dropping a multi-GB file into another agent's inbox or registration
// path: without a cap, ReadRegistration / inbox reads would OOM the consumer.
const (
	MaxRegistrationBytes = 1 << 20 // 1 MiB — registrations are tiny in practice.
	MaxInboxMessageBytes = 4 << 20 // 4 MiB — free-form markdown bodies.

	CurrentProtocolVersion = 2
)

// ReadFileLimit reads up to max bytes from path, returning ErrFileTooLarge
// (wrapped with the path) if the file exceeds the cap. Used wherever a peer
// agent could plant a file we are obligated to read.
//
// The open is no-follow (O_NOFOLLOW on unix) and the regular-file check runs
// against the OPENED fd (fstat), not a separate Lstat of the path. This closes
// the Lstat→Open TOCTOU window where a peer swaps a vetted regular file for a
// symlink between the check and the read. On unix the guarantee is structural;
// on Windows (no portable O_NOFOLLOW) it degrades to a best-effort Lstat +
// open, see openRegularNoFollow.
func ReadFileLimit(path string, max int64) ([]byte, error) {
	f, err := openRegularNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("%s: file exceeds %d-byte limit", path, max)
	}
	return data, nil
}

// Registration is the parsed live agent registration frontmatter plus body.
//
// Pull-only (simple-again Gate 6c): a registration publishes NO wake state.
// Delivery is inbox-directory + atomic write. Reachability is derived from
// LastSeen (age vs the pool's stale_after / gate's stricter StaleRegThreshold)
// plus, where it matters, a live serve claim (v2.5 plan B5 — .live, the
// detached poller, session ancestry, and the status/provenance extras are all
// gone; LastSeen + the serve claim are now the only freshness sources).
type Registration struct {
	AgentID         string
	ProtocolVersion int
	Vendor          string
	ControlRepo     string
	WorkingRepos    []string
	Host            string
	LastSeen        time.Time

	Body string
}

// ReadRegistration parses an agentchute live registration file.
func ReadRegistration(path string) (*Registration, error) {
	data, err := ReadFileLimit(path, MaxRegistrationBytes)
	if err != nil {
		return nil, err
	}

	fields, body, err := parseFrontmatter(string(data))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	reg := &Registration{
		AgentID:         fields.scalar("agent_id"),
		ProtocolVersion: parseProtocolVersion(fields.scalar("v")),
		Vendor:          fields.scalar("vendor"),
		ControlRepo:     fields.scalar("control_repo"),
		WorkingRepos:    fields.list("working_repos"),
		Host:            fields.scalar("host"),
		Body:            body,
	}

	if lastSeen := fields.scalar("last_seen"); lastSeen != "" {
		parsed, err := parseTimestamp(lastSeen)
		if err != nil {
			return nil, fmt.Errorf("last_seen: %w", err)
		}
		reg.LastSeen = parsed
	}

	// v2.5 plan B5 dual-read courtesy: status/restart_at/last_active/
	// launched_by/shim_name/hook_event are retired fields. An OLD row still
	// carries them on disk for one release — parseFrontmatter already parses
	// them into `fields` as ordinary key:value pairs; simply not reading them
	// here is what "tolerate and drop" means. No explicit ignore-list or
	// error path is needed: an old row's presence of these keys was never
	// itself a parse failure, so removing the reads is the entire migration.

	if err := reg.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return reg, nil
}

// WriteRegistration writes a registration with atomic temp-file replacement.
func WriteRegistration(path string, r *Registration) error {
	if err := r.Validate(); err != nil {
		return err
	}
	return atomicWriteFile(path, []byte(formatRegistration(r)))
}

// WriteRegistrationExclusive writes a fresh registration and fails with
// os.ErrExist if the path already exists. Registration startup uses it so a
// concurrent creator is re-read before a same-id merge is allowed.
//
// The destination is published atomically: content is written to a temp file
// first, then hard-linked into place. os.Link fails with EEXIST (recognized by
// os.IsExist) when the target already exists, preserving exclusive semantics —
// but unlike an O_EXCL create followed by a separate write, the visible file is
// never observed empty. A losing racer that reads the just-created
// registration must see its full record before deciding whether it may adopt
// the id.
func WriteRegistrationExclusive(path string, r *Registration) error {
	if err := r.Validate(); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".tmp_"+filepath.Base(path)+"_")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // best-effort: removed after link, or on any failure.

	if _, err := tmp.WriteString(formatRegistration(r)); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Link(tmpName, path); err != nil {
		return err // EEXIST surfaces as os.IsExist for the registration retry.
	}
	_ = syncDir(dir)
	return nil
}

// HeartbeatRegistration is the lease-gated, unconditional per-tick refresh of
// a registration row (v2.5 plan B1, C13). It replaces UpdateLastSeen: instead
// of a CLI touch refreshing an existing row (and erroring if the row is
// gone), only a live `serve` process advances LastSeen, and it self-heals — a
// row that SweepStaleRegistrations removed, or one that never existed, is
// recreated fresh from template rather than erroring.
//
// SERVE-ONLY: an empty leaseToken is a caller bug (unit tests constructing one
// directly aside), not a legitimate "no lease" case — only a process holding
// a live serve lease should ever be advancing a row's LastSeen.
//
// Fencing: VerifyFence runs FIRST, inside the SAME WithAgentLock(id) critical
// section as the read-merge-write, so a holder reclaimed between ticks (e.g.
// a paused-then-resumed laptop) writes NOTHING on ErrFenced — it cannot
// resurrect a row another serve now owns.
//
// Merge: template wins for every field EXCEPT Body and WorkingRepos, which
// the on-disk row wins for when one exists — those are user/other-command-
// owned (`register`/`boot --working-repo`, a hand-edited bio) and must
// survive ticks the runner's own template has no opinion on. When no row
// exists yet, template is written as-is.
func HeartbeatRegistration(cfg *Config, template Registration, leaseToken string) error {
	if leaseToken == "" {
		return fmt.Errorf("HeartbeatRegistration: empty lease token (heartbeat is serve-only)")
	}
	if err := ValidateAgentID(template.AgentID); err != nil {
		return err
	}
	return withAgentLock(cfg, template.AgentID, func() error {
		if err := VerifyFence(cfg, template.AgentID, leaseToken); err != nil {
			return err
		}
		path := cfg.AgentRegistrationPath(template.AgentID)
		reg := template
		if existing, err := ReadRegistration(path); err == nil {
			reg.Body = existing.Body
			reg.WorkingRepos = existing.WorkingRepos
		} else if !os.IsNotExist(err) {
			return err
		}
		reg.LastSeen = time.Now().UTC()
		return WriteRegistration(path, &reg)
	})
}

// Validate checks the fields required by the v1 registration format.
func (r *Registration) Validate() error {
	if r == nil {
		return fmt.Errorf("registration is nil")
	}
	if err := ValidateAgentID(r.AgentID); err != nil {
		return err
	}
	if strings.TrimSpace(r.Vendor) == "" {
		return fmt.Errorf("vendor is required")
	}
	if strings.TrimSpace(r.ControlRepo) == "" {
		return fmt.Errorf("control_repo is required")
	}
	if !filepath.IsAbs(r.ControlRepo) {
		return fmt.Errorf("control_repo %q must be an absolute path", r.ControlRepo)
	}
	for _, repo := range r.WorkingRepos {
		if !filepath.IsAbs(repo) {
			return fmt.Errorf("working_repos entry %q must be an absolute path", repo)
		}
	}
	if r.LastSeen.IsZero() {
		return fmt.Errorf("last_seen is required")
	}
	return nil
}

type frontmatterFields map[string]fieldValue

type fieldValue struct {
	scalar string
	list   []string
}

func (f frontmatterFields) scalar(key string) string {
	return f[key].scalar
}

func (f frontmatterFields) list(key string) []string {
	values := f[key].list
	if len(values) == 0 {
		return nil
	}
	return append([]string(nil), values...)
}

func parseFrontmatter(data string) (frontmatterFields, string, error) {
	text := strings.ReplaceAll(data, "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return nil, "", fmt.Errorf("missing frontmatter opening ---")
	}

	closing := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			closing = i
			break
		}
	}
	if closing == -1 {
		return nil, "", fmt.Errorf("missing frontmatter closing ---")
	}

	fields := make(frontmatterFields)
	for i := 1; i < closing; i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			return nil, "", fmt.Errorf("unexpected indented line %q", line)
		}

		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, "", fmt.Errorf("invalid frontmatter line %q", line)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" {
			return nil, "", fmt.Errorf("empty frontmatter key")
		}
		if _, exists := fields[key]; exists {
			return nil, "", fmt.Errorf("duplicate frontmatter key %q", key)
		}

		if value != "" {
			fields[key] = fieldValue{scalar: cleanScalar(value)}
			continue
		}

		var items []string
		for i+1 < closing {
			next := lines[i+1]
			trimmed := strings.TrimSpace(next)
			if trimmed == "" {
				i++
				continue
			}
			if strings.HasPrefix(trimmed, "- ") {
				items = append(items, cleanScalar(strings.TrimSpace(strings.TrimPrefix(trimmed, "- "))))
				i++
				continue
			}
			break
		}
		fields[key] = fieldValue{list: items}
	}

	body := strings.Join(lines[closing+1:], "\n")
	body = strings.TrimPrefix(body, "\n")
	return fields, body, nil
}

func formatRegistration(r *Registration) string {
	var b strings.Builder
	b.WriteString("---\n")
	writeScalar(&b, "agent_id", r.AgentID)
	if r.ProtocolVersion != 0 {
		writeScalar(&b, "v", strconv.Itoa(r.ProtocolVersion))
	}
	writeScalar(&b, "vendor", r.Vendor)
	writeScalar(&b, "control_repo", r.ControlRepo)
	if len(r.WorkingRepos) > 0 {
		b.WriteString("working_repos:\n")
		for _, repo := range r.WorkingRepos {
			b.WriteString("  - ")
			b.WriteString(quoteIfNeeded(repo))
			b.WriteString("\n")
		}
	}
	if r.Host != "" {
		writeScalar(&b, "host", r.Host)
	}
	writeScalar(&b, "last_seen", formatTimestamp(r.LastSeen))
	b.WriteString("---\n")
	if strings.TrimSpace(r.Body) != "" {
		b.WriteString("\n")
		b.WriteString(strings.TrimPrefix(r.Body, "\n"))
		if !strings.HasSuffix(r.Body, "\n") {
			b.WriteString("\n")
		}
	}
	return b.String()
}

func parseProtocolVersion(value string) int {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return -1
	}
	return n
}

func writeScalar(b *strings.Builder, key, value string) {
	b.WriteString(key)
	b.WriteString(": ")
	b.WriteString(quoteIfNeeded(value))
	b.WriteString("\n")
}

func cleanScalar(value string) string {
	value = strings.TrimSpace(value)
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

func quoteIfNeeded(value string) string {
	if value == "" {
		return `""`
	}
	if strings.ContainsAny(value, " \t:#'\"\n\r\x00") || strings.HasPrefix(value, "%") {
		return strconv.Quote(value)
	}
	return value
}

func parseTimestamp(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, err
	}
	return parsed.UTC(), nil
}

func formatTimestamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

func atomicWriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := ensurePrivateDir(dir); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".tmp_"+filepath.Base(path)+"_")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	// The temp file no longer exists under tmpName (rename consumed it) and the
	// new content is already live at path. Clear cleanup BEFORE syncDir so a
	// syncDir failure cannot trigger the deferred os.Remove(tmpName) — which
	// would now resolve to the published target's old inode in some fs races —
	// and so the published content is never treated as unwritten. The syncDir
	// error is still returned: the write succeeded but the dir-entry durability
	// barrier did not, which the caller may want to know about.
	cleanup = false
	if err := syncDir(dir); err != nil {
		return err
	}
	return nil
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// RegistrationReadError pairs a registration file path with the error that
// prevented it from parsing, for callers iterating over the agents directory
// with the lenient reader.
type RegistrationReadError struct {
	Path string
	Err  error
}

// Error renders a RegistrationReadError in "<path>: <err>" shape so callers
// can log/warn each entry uniformly.
func (e RegistrationReadError) Error() string {
	return fmt.Sprintf("%s: %v", e.Path, e.Err)
}

// ReadRegistrationsLenient reads every conforming *.md registration file in
// dir and returns the parseable registrations alongside per-file errors for
// the rest. README.md, dotfiles, and *.example.md are silently skipped (per
// the existing layout convention).
//
// Use this when one bad registration must NOT abort a multi-peer scan —
// notably identity checks and the update/setup
// re-sync (`update`), which enumerate every peer and must log/warn a single
// unparseable entry and continue. Strict callers (single-registration ops,
// the `status` command) should keep using ReadRegistration directly.
//
// A nil-or-missing dir returns (nil, nil) for callers that want to treat
// "no agents/ yet" as a clean empty result; any other dir-level error is
// surfaced as a single RegistrationReadError with the dir as Path.
func ReadRegistrationsLenient(dir string) ([]*Registration, []RegistrationReadError) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, []RegistrationReadError{{Path: dir, Err: err}}
	}
	var regs []*Registration
	var errs []RegistrationReadError
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() ||
			strings.HasPrefix(name, ".") ||
			!strings.HasSuffix(name, ".md") ||
			strings.HasSuffix(name, ".example.md") ||
			name == "README.md" {
			continue
		}
		path := filepath.Join(dir, name)
		reg, err := ReadRegistration(path)
		if err != nil {
			errs = append(errs, RegistrationReadError{Path: path, Err: err})
			continue
		}
		regs = append(regs, reg)
	}
	return regs, errs
}

// RegistrationsByAgentID returns a deterministic map key order for callers that
// want stable status output.
func RegistrationsByAgentID(regs map[string]*Registration) []string {
	keys := make([]string, 0, len(regs))
	for key := range regs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
