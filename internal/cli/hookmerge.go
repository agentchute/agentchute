package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Hook files come in two kinds (opus-xhigh S3).
//
// A SETTINGS file (.claude/settings.json, .gemini/settings.json) is the
// project's own settings: permissions, env, MCP servers, its own hooks.
// agentchute owns only its hook entries (any hook whose command invokes
// agentchute) and the permission rules its template lists, so a write MERGES
// those into the file and keeps every other key, in document order.
//
// A DEDICATED hook file (.codex/hooks.json, .agents/hooks.json) holds only
// agentchute's hooks and is replaced whole: codex trusts hooks by position, so
// its layout must stay the template's.
//
// Either way a write keeps a timestamped backup of what it replaced, and serve
// never rewrites an existing file — only setup and `hooks install --force`
// repair one.

// settingsHookWrappers are the wrappers whose hook file is a settings file.
var settingsHookWrappers = map[string]bool{"claude-code": true, "gemini-cli": true}

type hookFileState int

const (
	hookFileCurrent hookFileState = iota // installed, and agentchute's part matches the template
	hookFileMissing                      // not installed
	hookFileStale                        // installed, agentchute's part differs; Proposed is the repair
	hookFileInvalid                      // installed but cannot be merged (invalid JSON, wrong shape)
)

// hookFilePlan is what an install or check would do to one wrapper's hook
// file, computed without writing anything.
type hookFilePlan struct {
	Wrapper  hookWrapper
	Dest     string
	State    hookFileState
	Existing []byte // nil when missing
	Proposed []byte // what a repair or create writes: the template, or the merge
	Merge    bool   // Proposed merges agentchute's entries into existing settings
	Problem  error  // why the file is invalid
}

// planHookFile classifies w's hook file under root. A read error other than
// "does not exist" (a directory in the way, a permission problem) is returned
// as an error: there is nothing safe to plan against.
func planHookFile(w hookWrapper, root string) (hookFilePlan, error) {
	tmpl, err := fs.ReadFile(hooksFS, w.Src)
	if err != nil {
		return hookFilePlan{}, fmt.Errorf("read embedded template for %s: %w", w.Name, err)
	}
	dest := filepath.Join(root, w.Dest)
	p := hookFilePlan{Wrapper: w, Dest: dest}
	existing, err := os.ReadFile(dest)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			p.State = hookFileMissing
			p.Proposed = tmpl
			return p, nil
		}
		return hookFilePlan{}, fmt.Errorf("read %s: %w", dest, err)
	}
	p.Existing = existing
	if bytes.Equal(existing, tmpl) {
		p.State = hookFileCurrent
		return p, nil
	}
	if !settingsHookWrappers[w.Name] {
		p.State = hookFileStale
		p.Proposed = tmpl
		return p, nil
	}
	merged, current, err := mergeSettingsHookFile(existing, tmpl)
	switch {
	case err != nil:
		p.State = hookFileInvalid
		p.Problem = err
	case current:
		p.State = hookFileCurrent
	default:
		p.State = hookFileStale
		p.Proposed = merged
		p.Merge = true
	}
	return p, nil
}

// mergeSettingsHookFile merges the template's agentchute entries into an
// existing settings file. current reports that agentchute's part already
// matches (merged is then nil and nothing needs writing).
func mergeSettingsHookFile(existing, tmpl []byte) (merged []byte, current bool, err error) {
	tv, err := decodeOrderedJSON(tmpl)
	if err != nil {
		return nil, false, fmt.Errorf("embedded template: %w", err)
	}
	t, ok := tv.(*jsonObject)
	if !ok {
		return nil, false, errors.New("embedded template is not a JSON object")
	}
	var e *jsonObject
	if len(bytes.TrimSpace(existing)) == 0 {
		return tmpl, false, nil
	}
	ev, err := decodeOrderedJSON(existing)
	if err != nil {
		return nil, false, fmt.Errorf("not valid JSON: %w", err)
	}
	if e, ok = ev.(*jsonObject); !ok {
		return nil, false, errors.New("the top level is not a JSON object")
	}
	if current, err = settingsHookPartCurrent(e, t); err != nil || current {
		return nil, current, err
	}
	// A file holding nothing but agentchute's own entries (an earlier
	// template, or this one hand-edited) is not the project's: it becomes the
	// template byte for byte, so setup-managed files stay canonical.
	if onlyAgentchuteSettings(e, t) {
		return tmpl, false, nil
	}
	if err := mergeSettingsInto(e, t); err != nil {
		return nil, false, err
	}
	out, err := encodeOrderedJSON(e)
	return out, false, err
}

// settingsHookPartCurrent reports whether agentchute's part of e matches t:
// the same agentchute hooks in the same groups, every template permission
// rule present. Extra project rules do not imply drift, even if an older
// template once shipped the same string: there is no ownership evidence.
func settingsHookPartCurrent(e, t *jsonObject) (bool, error) {
	ev, err := agentchuteHookView(e)
	if err != nil {
		return false, err
	}
	tv, err := agentchuteHookView(t)
	if err != nil {
		return false, err
	}
	eb, _ := json.Marshal(ev)
	tb, _ := json.Marshal(tv)
	if !bytes.Equal(eb, tb) {
		return false, nil
	}
	tpv, ok := t.get("permissions")
	if !ok {
		return true, nil
	}
	tp, ok := tpv.(*jsonObject)
	if !ok {
		return false, errors.New(`embedded template "permissions" is not an object`)
	}
	ep, err := objectField(e, "permissions")
	if err != nil {
		return false, err
	}
	for _, list := range tp.keys {
		have, err := arrayField(ep, list)
		if err != nil {
			return false, err
		}
		want, _ := tp.vals[list].([]any)
		for _, rule := range want {
			if !containsJSONString(have, rule) {
				return false, nil
			}
		}
	}
	return true, nil
}

// mergeSettingsInto rewrites e in place: every agentchute hook is removed
// wherever it sits (a group left empty by that is dropped, and an event left
// empty), the template's hook groups are appended to their events, and missing
// template permission rules appended. Existing permission rules are retained.
// Keys the template does not mention are never touched.
func mergeSettingsInto(e, t *jsonObject) error {
	for _, key := range t.keys {
		switch key {
		case "hooks":
			eh, err := objectField(e, "hooks")
			if err != nil {
				return err
			}
			if eh == nil {
				eh = newJSONObject()
				e.set("hooks", eh)
			}
			if err := stripAgentchuteHooks(eh); err != nil {
				return err
			}
			th, ok := t.vals["hooks"].(*jsonObject)
			if !ok {
				return errors.New(`embedded template "hooks" is not an object`)
			}
			for _, event := range th.keys {
				cur, err := arrayField(eh, event)
				if err != nil {
					return err
				}
				add, _ := th.vals[event].([]any)
				eh.set(event, append(cur, add...))
			}
		case "permissions":
			ep, err := objectField(e, "permissions")
			if err != nil {
				return err
			}
			if ep == nil {
				ep = newJSONObject()
				e.set("permissions", ep)
			}
			tp, ok := t.vals["permissions"].(*jsonObject)
			if !ok {
				return errors.New(`embedded template "permissions" is not an object`)
			}
			for _, list := range tp.keys {
				cur, err := arrayField(ep, list)
				if err != nil {
					return err
				}
				out := append([]any{}, cur...)
				want, _ := tp.vals[list].([]any)
				for _, rule := range want {
					if !containsJSONString(out, rule) {
						out = append(out, rule)
					}
				}
				ep.set(list, out)
			}
		default:
			if _, ok := e.get(key); !ok {
				e.set(key, t.vals[key])
			}
		}
	}
	return nil
}

// onlyAgentchuteSettings reports whether e holds nothing a project put there:
// every hook invokes agentchute, every permission rule is still in the current
// template, and no other key differs from the template's. A retired rule can
// be user-owned, so it must prevent the whole-file replacement shortcut.
func onlyAgentchuteSettings(e, t *jsonObject) bool {
	for _, key := range e.keys {
		switch key {
		case "hooks":
			hooks, ok := e.vals[key].(*jsonObject)
			if !ok {
				return false
			}
			for _, event := range hooks.keys {
				groups, ok := hooks.vals[event].([]any)
				if !ok {
					return false
				}
				for _, g := range groups {
					group, ok := g.(*jsonObject)
					if !ok {
						return false
					}
					list, _ := group.vals["hooks"].([]any)
					for _, h := range list {
						if !agentchuteOwnedHook(h) {
							return false
						}
					}
				}
			}
		case "permissions":
			perms, ok := e.vals[key].(*jsonObject)
			if !ok {
				return false
			}
			tp, _ := t.vals["permissions"].(*jsonObject)
			for _, list := range perms.keys {
				rules, ok := perms.vals[list].([]any)
				if !ok {
					return false
				}
				var owned []any
				if tp != nil {
					owned, _ = tp.vals[list].([]any)
				}
				for _, rule := range rules {
					if containsJSONString(owned, rule) {
						continue
					}
					return false
				}
			}
		default:
			tv, ok := t.get(key)
			if !ok {
				return false
			}
			a, _ := json.Marshal(plainJSON(e.vals[key]))
			b, _ := json.Marshal(plainJSON(tv))
			if !bytes.Equal(a, b) {
				return false
			}
		}
	}
	return true
}

// stripAgentchuteHooks removes every agentchute hook from a hooks object.
func stripAgentchuteHooks(hooks *jsonObject) error {
	for _, event := range append([]string(nil), hooks.keys...) {
		groups, ok := hooks.vals[event].([]any)
		if !ok {
			return fmt.Errorf(`"hooks.%s" is not an array`, event)
		}
		kept := make([]any, 0, len(groups))
		removedAny := false
		for _, g := range groups {
			group, ok := g.(*jsonObject)
			if !ok {
				kept = append(kept, g)
				continue
			}
			list, ok := group.vals["hooks"].([]any)
			if !ok {
				kept = append(kept, g)
				continue
			}
			remaining := make([]any, 0, len(list))
			for _, h := range list {
				if !agentchuteOwnedHook(h) {
					remaining = append(remaining, h)
				}
			}
			if len(remaining) == len(list) {
				kept = append(kept, g)
				continue
			}
			removedAny = true
			if len(remaining) == 0 {
				continue
			}
			group.set("hooks", remaining)
			kept = append(kept, group)
		}
		if removedAny && len(kept) == 0 {
			hooks.del(event)
			continue
		}
		hooks.set(event, kept)
	}
	return nil
}

// agentchuteHookView is the part of a settings file's hooks agentchute owns:
// per event, each group holding at least one agentchute hook, reduced to its
// matcher and those hooks, as plain values json.Marshal can compare.
func agentchuteHookView(root *jsonObject) (map[string][]any, error) {
	view := map[string][]any{}
	hooks, err := objectField(root, "hooks")
	if err != nil || hooks == nil {
		return view, err
	}
	for _, event := range hooks.keys {
		groups, ok := hooks.vals[event].([]any)
		if !ok {
			return nil, fmt.Errorf(`"hooks.%s" is not an array`, event)
		}
		for _, g := range groups {
			group, ok := g.(*jsonObject)
			if !ok {
				continue
			}
			list, _ := group.vals["hooks"].([]any)
			var ours []any
			for _, h := range list {
				if agentchuteOwnedHook(h) {
					ours = append(ours, plainJSON(h))
				}
			}
			if len(ours) == 0 {
				continue
			}
			entry := map[string]any{"hooks": ours}
			if m, ok := group.get("matcher"); ok {
				entry["matcher"] = plainJSON(m)
			}
			view[event] = append(view[event], entry)
		}
	}
	return view, nil
}

// agentchuteOwnedHook reports whether a hook entry's command invokes
// agentchute in any of the forms the templates and doctor recognize.
func agentchuteOwnedHook(h any) bool {
	obj, ok := h.(*jsonObject)
	if !ok {
		return false
	}
	cmd, _ := obj.vals["command"].(string)
	return hookSubcmdTokenRE.MatchString(cmd)
}

// objectField returns e[key] as an object, nil when absent, or an error when
// it is present with another type.
func objectField(e *jsonObject, key string) (*jsonObject, error) {
	if e == nil {
		return nil, nil
	}
	v, ok := e.get(key)
	if !ok {
		return nil, nil
	}
	obj, ok := v.(*jsonObject)
	if !ok {
		return nil, fmt.Errorf("%q is not a JSON object", key)
	}
	return obj, nil
}

// arrayField returns e[key] as an array, nil when absent, or an error when it
// is present with another type.
func arrayField(e *jsonObject, key string) ([]any, error) {
	if e == nil {
		return nil, nil
	}
	v, ok := e.get(key)
	if !ok {
		return nil, nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%q is not a JSON array", key)
	}
	return arr, nil
}

func containsJSONString(list []any, v any) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	for _, item := range list {
		if other, ok := item.(string); ok && other == s {
			return true
		}
	}
	return false
}

// writeHookFile applies a plan: a timestamped backup of the existing file
// (when there is one), then an atomic temp+rename write of Proposed. The
// parent directory is created 0700; an existing file keeps its mode.
func writeHookFile(p hookFilePlan, now time.Time) (backup string, err error) {
	parent := filepath.Dir(p.Dest)
	// 0700 parent, as before this file learned to merge: MkdirAll only sets
	// the mode on directories it creates, so tighten explicitly (codex
	// review #3 on the original installer).
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", fmt.Errorf("mkdir parent for %s: %w", p.Dest, err)
	}
	if err := os.Chmod(parent, 0o700); err != nil {
		return "", fmt.Errorf("chmod parent for %s: %w", p.Dest, err)
	}
	mode := os.FileMode(0o600)
	if p.Existing != nil {
		if info, err := os.Stat(p.Dest); err == nil {
			mode = info.Mode().Perm()
		}
		if backup, err = writeHookBackup(p.Dest, p.Existing, now); err != nil {
			return "", fmt.Errorf("write backup for %s: %w", p.Dest, err)
		}
	}
	tmp, err := os.CreateTemp(parent, ".tmp_hook-*")
	if err != nil {
		return "", fmt.Errorf("create temp for %s: %w", p.Dest, err)
	}
	name := tmp.Name()
	if _, err := tmp.Write(p.Proposed); err != nil {
		tmp.Close()
		os.Remove(name)
		return "", fmt.Errorf("write temp for %s: %w", p.Dest, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return "", fmt.Errorf("close temp for %s: %w", p.Dest, err)
	}
	if err := os.Chmod(name, mode); err != nil {
		os.Remove(name)
		return "", fmt.Errorf("chmod temp for %s: %w", p.Dest, err)
	}
	if err := os.Rename(name, p.Dest); err != nil {
		os.Remove(name)
		return "", fmt.Errorf("rename %s → %s: %w", name, p.Dest, err)
	}
	return backup, nil
}

// hookBackupSuffix names the backups writeHookFile keeps beside a hook file.
const hookBackupSuffix = ".agentchute-backup-"

// writeHookBackup copies data to <dest>.agentchute-backup-<UTC stamp>, adding
// -2, -3, … when a backup from the same second already exists; one backup per
// write, never overwritten, so a later repair cannot destroy an earlier one.
func writeHookBackup(dest string, data []byte, now time.Time) (string, error) {
	base := dest + hookBackupSuffix + now.UTC().Format("20060102T150405Z")
	for i := 1; i <= 1000; i++ {
		path := base
		if i > 1 {
			path = base + "-" + strconv.Itoa(i)
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		_, werr := f.Write(data)
		cerr := f.Close()
		if werr != nil || cerr != nil {
			os.Remove(path)
			if werr != nil {
				return "", werr
			}
			return "", cerr
		}
		return path, nil
	}
	return "", fmt.Errorf("more than 1000 backups named %s*", base)
}

// ---------- order-preserving JSON ----------

// jsonObject is a JSON object that keeps its keys in document order, so a
// merged settings file differs from the project's only where agentchute's
// entries changed.
type jsonObject struct {
	keys []string
	vals map[string]any
}

func newJSONObject() *jsonObject { return &jsonObject{vals: map[string]any{}} }

func (o *jsonObject) get(k string) (any, bool) {
	v, ok := o.vals[k]
	return v, ok
}

// set replaces k's value in place, or appends k when it is new.
func (o *jsonObject) set(k string, v any) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

func (o *jsonObject) del(k string) {
	if _, ok := o.vals[k]; !ok {
		return
	}
	delete(o.vals, k)
	for i, key := range o.keys {
		if key == k {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			return
		}
	}
}

// decodeOrderedJSON decodes one JSON value: objects as *jsonObject, arrays as
// []any, numbers as json.Number. A duplicate key is refused — which value the
// harness would use is not agentchute's to guess.
func decodeOrderedJSON(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := decodeOrderedValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("unexpected data after the JSON value")
	}
	return v, nil
}

func decodeOrderedValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return tok, nil
	}
	switch delim {
	case '{':
		obj := newJSONObject()
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			key, ok := kt.(string)
			if !ok {
				return nil, errors.New("object key is not a string")
			}
			if _, dup := obj.vals[key]; dup {
				return nil, fmt.Errorf("duplicate key %q", key)
			}
			val, err := decodeOrderedValue(dec)
			if err != nil {
				return nil, err
			}
			obj.set(key, val)
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		return obj, nil
	case '[':
		arr := []any{}
		for dec.More() {
			v, err := decodeOrderedValue(dec)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		return arr, nil
	}
	return nil, fmt.Errorf("unexpected delimiter %q", delim)
}

// encodeOrderedJSON writes v the way json.MarshalIndent(v, "", "  ") would,
// keys in document order and without HTML escaping, plus a final newline —
// the layout every shipped template uses.
func encodeOrderedJSON(v any) ([]byte, error) {
	var b bytes.Buffer
	if err := writeOrderedValue(&b, v, ""); err != nil {
		return nil, err
	}
	b.WriteByte('\n')
	return b.Bytes(), nil
}

func writeOrderedValue(b *bytes.Buffer, v any, indent string) error {
	inner := indent + "  "
	switch t := v.(type) {
	case *jsonObject:
		if len(t.keys) == 0 {
			b.WriteString("{}")
			return nil
		}
		b.WriteString("{\n")
		for i, k := range t.keys {
			b.WriteString(inner)
			writeJSONString(b, k)
			b.WriteString(": ")
			if err := writeOrderedValue(b, t.vals[k], inner); err != nil {
				return err
			}
			if i < len(t.keys)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		b.WriteString(indent)
		b.WriteByte('}')
	case []any:
		if len(t) == 0 {
			b.WriteString("[]")
			return nil
		}
		b.WriteString("[\n")
		for i, item := range t {
			b.WriteString(inner)
			if err := writeOrderedValue(b, item, inner); err != nil {
				return err
			}
			if i < len(t)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		b.WriteString(indent)
		b.WriteByte(']')
	case string:
		writeJSONString(b, t)
	case json.Number:
		b.WriteString(t.String())
	case bool:
		b.WriteString(strconv.FormatBool(t))
	case nil:
		b.WriteString("null")
	default:
		return fmt.Errorf("unsupported JSON value of type %T", v)
	}
	return nil
}

func writeJSONString(b *bytes.Buffer, s string) {
	var tmp bytes.Buffer
	enc := json.NewEncoder(&tmp)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	b.Write(bytes.TrimSuffix(tmp.Bytes(), []byte{'\n'}))
}

// plainJSON converts an ordered value to plain maps and slices, so
// json.Marshal can compare two values with object key order ignored.
func plainJSON(v any) any {
	switch t := v.(type) {
	case *jsonObject:
		m := make(map[string]any, len(t.keys))
		for _, k := range t.keys {
			m[k] = plainJSON(t.vals[k])
		}
		return m
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = plainJSON(item)
		}
		return out
	default:
		return v
	}
}
