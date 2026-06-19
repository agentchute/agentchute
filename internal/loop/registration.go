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
)

// ReadFileLimit reads up to max bytes from path, returning ErrFileTooLarge
// (wrapped with the path) if the file exceeds the cap. Used wherever a peer
// agent could plant a file we are obligated to read.
func ReadFileLimit(path string, max int64) ([]byte, error) {
	if err := ensureRegularFile(path); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
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

func ensureRegularFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	mode := info.Mode()
	if mode&os.ModeSymlink != 0 {
		return fmt.Errorf("%s: symlink not allowed", path)
	}
	if !mode.IsRegular() {
		return fmt.Errorf("%s: not a regular file", path)
	}
	return nil
}

type Status string

const (
	StatusActive    Status = "active"
	StatusExhausted Status = "exhausted"
	StatusOffline   Status = "offline"
)

// Registration is the parsed live agent registration frontmatter plus body.
type Registration struct {
	AgentID      string
	Vendor       string
	ControlRepo  string
	WorkingRepos []string
	Host         string
	WakeMethod   string
	WakeTarget   string
	LastSeen     time.Time
	Status       Status
	RestartAt    *time.Time
	LastActive   *time.Time
	Body         string
}

// IsPokable reports whether senders can dispatch a poke for this registration.
// Both wake_method and wake_target must be present; either empty means the
// recipient is responsible for its own polling cadence.
func (r *Registration) IsPokable() bool {
	if r == nil {
		return false
	}
	return strings.TrimSpace(r.WakeMethod) != "" && strings.TrimSpace(r.WakeTarget) != ""
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
		AgentID:      fields.scalar("agent_id"),
		Vendor:       fields.scalar("vendor"),
		ControlRepo:  fields.scalar("control_repo"),
		WorkingRepos: fields.list("working_repos"),
		Host:         fields.scalar("host"),
		WakeMethod:   fields.scalar("wake_method"),
		WakeTarget:   fields.scalar("wake_target"),
		Status:       Status(fields.scalar("status")),
		Body:         body,
	}
	if reg.Status == "" {
		reg.Status = StatusActive
	}

	if lastSeen := fields.scalar("last_seen"); lastSeen != "" {
		parsed, err := parseTimestamp(lastSeen)
		if err != nil {
			return nil, fmt.Errorf("last_seen: %w", err)
		}
		reg.LastSeen = parsed
	}

	if restartAt := fields.scalar("restart_at"); restartAt != "" {
		parsed, err := parseTimestamp(restartAt)
		if err != nil {
			return nil, fmt.Errorf("restart_at: %w", err)
		}
		reg.RestartAt = &parsed
	}

	if lastActive := fields.scalar("last_active"); lastActive != "" {
		parsed, err := parseTimestamp(lastActive)
		if err != nil {
			return nil, fmt.Errorf("last_active: %w", err)
		}
		reg.LastActive = &parsed
	}

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
// os.ErrExist if the path already exists. Used by contextual identity startup
// so two simultaneous agents do not silently claim the same first ID.
//
// The destination is published atomically: content is written to a temp file
// first, then hard-linked into place. os.Link fails with EEXIST (recognized by
// os.IsExist) when the target already exists, preserving exclusive semantics —
// but unlike an O_EXCL create followed by a separate write, the visible file is
// never observed empty. That matters under the SessionStart race: a losing
// racer that reads the just-created same-pane registration must see its full
// wake_target to adopt it instead of suffixing.
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
		return err // EEXIST surfaces as os.IsExist for the contextual-collision loop.
	}
	_ = syncDir(dir)
	return nil
}

// UpdateLastSeen updates last_seen via the same structured path as other
// registration writes. The read-modify-write runs under the per-agent lock so
// a concurrent status mutation (e.g. the runner marking itself offline) is not
// clobbered by a stale-read overwrite, and two concurrent updaters cannot tear
// the registration file.
func UpdateLastSeen(cfg *Config, agentID string, t time.Time) error {
	return withAgentLock(cfg, agentID, func() error {
		path := cfg.AgentRegistrationPath(agentID)
		reg, err := ReadRegistration(path)
		if err != nil {
			return err
		}
		reg.LastSeen = t.UTC()
		return WriteRegistration(path, reg)
	})
}

// UpdateLastActive updates last_active via the same structured path as other
// registration writes. Runs under the per-agent lock for the same lost-update
// reasons as UpdateLastSeen.
func UpdateLastActive(cfg *Config, agentID string, t time.Time) error {
	return withAgentLock(cfg, agentID, func() error {
		path := cfg.AgentRegistrationPath(agentID)
		reg, err := ReadRegistration(path)
		if err != nil {
			return err
		}
		lastActive := t.UTC()
		reg.LastActive = &lastActive
		return WriteRegistration(path, reg)
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
	if !validStatus(r.Status) {
		return fmt.Errorf("status must be one of %q, %q, %q", StatusActive, StatusExhausted, StatusOffline)
	}
	// AGENTCHUTE.md §5: wake_target is required when wake_method is set.
	method := strings.TrimSpace(r.WakeMethod)
	target := strings.TrimSpace(r.WakeTarget)
	if method != "" && target == "" {
		return fmt.Errorf("wake_target is required when wake_method=%q is set", method)
	}
	if method == "" && target != "" {
		return fmt.Errorf("wake_target set without wake_method")
	}
	// Shape-validate the wake_target so a hand-written peer registration cannot
	// smuggle an injection-shaped target (foreign pane, leading-dash flag
	// confusion, newline) past the parser into a poke. The pure validator runs
	// here; recipient-binding for unix: sockets is enforced separately in the
	// poke path (it needs Config + recipientID).
	if method != "" {
		if err := ValidateWakeTarget(method, target); err != nil {
			return err
		}
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
	status := r.Status
	if status == "" {
		status = StatusActive
	}

	var b strings.Builder
	b.WriteString("---\n")
	writeScalar(&b, "agent_id", r.AgentID)
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
	writeScalar(&b, "wake_method", r.WakeMethod)
	writeScalar(&b, "wake_target", r.WakeTarget)
	writeScalar(&b, "last_seen", formatTimestamp(r.LastSeen))
	writeScalar(&b, "status", string(status))
	if r.RestartAt != nil {
		writeScalar(&b, "restart_at", formatTimestamp(*r.RestartAt))
	}
	if r.LastActive != nil {
		writeScalar(&b, "last_active", formatTimestamp(*r.LastActive))
	}
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

func validStatus(status Status) bool {
	switch status {
	case "", StatusActive, StatusExhausted, StatusOffline:
		return true
	default:
		return false
	}
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
// notably the watchdog (§10.4) and cooperative waking (§10.5), where the
// spec requires per-peer errors to log/warn and continue. Strict callers
// (single-registration ops, the `status` command) should keep using
// ReadRegistration directly.
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
