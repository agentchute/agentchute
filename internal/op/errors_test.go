package op

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"testing"

	"github.com/agentchute/agentchute/internal/loop"
)

// The event union's whole contract is "exactly one arm": a consumer switches on
// the arms and would silently drop, or double-render, an event that broke it.
func TestEventConstructorsSetExactlyOneArm(t *testing.T) {
	events := []Event{
		NewMessageEvent(MessageEvent{Filename: "f"}),
		NewNoteEvent(NoteWarn, "w"),
		NewNoteEvent(NoteInfo, "i"),
		NewOwedEvent(OwedEvent{Ref: "r"}),
		NewAckItemEvent(AckItemEvent{Filename: "f"}),
	}
	for i, ev := range events {
		if !ev.Valid() {
			t.Fatalf("constructor %d produced %d arms, want 1", i, ev.Arms())
		}
	}
	if (Event{}).Valid() {
		t.Fatal("the zero Event must not satisfy the union invariant")
	}
	if (Event{Note: &NoteEvent{}, Owed: &OwedEvent{}}).Valid() {
		t.Fatal("a two-armed Event must not satisfy the union invariant")
	}
}

// CodeFor is a function with a default arm precisely so no non-nil error can
// reach the wire uncoded. This pins every sentinel's code, the wrapped case, the
// unclassified case, and nil.
func TestCodeForCoversEverySentinelPlusDefault(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{nil, ""},
		{ErrNotRegistered, "E_NOT_REGISTERED"},
		{ErrRecipientUnknown, "E_RECIPIENT_UNKNOWN"},
		{ErrRecipientUnreadable, "E_RECIPIENT_UNREADABLE"},
		{ErrRecipientStale, "E_RECIPIENT_STALE"},
		{ErrRecipientRacing, "E_RECIPIENT_RACING"},
		{ErrFenced, "E_FENCED"},
		{ErrLeaseHeld, "E_LEASE_HELD"},
		{ErrOrder, "E_ORDER"},
		// Wrapped sentinels keep their code: ops wrap freely for context.
		{fmt.Errorf("deliver: %w", ErrFenced), "E_FENCED"},
		{staleAt(ErrRecipientRacing, &loop.ErrRecipientStale{To: "peer"}), "E_RECIPIENT_RACING"},
		// Unclassified I/O takes the default arm — including ErrInboxMissing,
		// which deliberately has no code of its own.
		{errors.New("disk on fire"), "E_HUB_IO"},
		{loop.ErrInboxMissing, "E_HUB_IO"},
		{os.ErrPermission, "E_HUB_IO"},
	}
	for _, tc := range cases {
		if got := CodeFor(tc.err); got != tc.want {
			t.Fatalf("CodeFor(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

// Both stale arms must stay reachable BOTH ways: errors.Is says which raise site
// produced it (two distinct wire codes), errors.As hands the renderer the
// fields the C29 text needs. Losing either breaks a shipped message.
func TestStaleArmsCarrySentinelAndCause(t *testing.T) {
	cause := &loop.ErrRecipientStale{To: "codex", Age: 90, Threshold: 3600}
	for _, arm := range []error{ErrRecipientStale, ErrRecipientRacing} {
		err := staleAt(arm, cause)
		if !errors.Is(err, arm) {
			t.Fatalf("%v lost its sentinel", err)
		}
		var got *loop.ErrRecipientStale
		if !errors.As(err, &got) || got.To != "codex" {
			t.Fatalf("%v lost the *loop.ErrRecipientStale cause", err)
		}
	}
	// The two arms must not be confusable with each other, or the hub emits
	// one code where the CLI renders the other wording.
	if errors.Is(staleAt(ErrRecipientStale, cause), ErrRecipientRacing) {
		t.Fatal("the preflight arm matched the under-lock sentinel")
	}
}

// The re-exported sentinels must be the SAME values internal/loop raises, so a
// loop error travelling through op still satisfies the CLI's existing checks.
func TestReExportedSentinelsAreLoopsOwnValues(t *testing.T) {
	pairs := []struct{ op, loopErr error }{
		{ErrRecipientUnknown, loop.ErrRecipientUnknown},
		{ErrRecipientUnreadable, loop.ErrRecipientUnreadable},
		{ErrFenced, loop.ErrFenced},
		{ErrLeaseHeld, loop.ErrLeaseHeld},
	}
	for _, p := range pairs {
		if p.op != p.loopErr {
			t.Fatalf("%v is a redeclared sentinel, not a re-export of %v", p.op, p.loopErr)
		}
	}
}

// The sentinel set is CLOSED at eight (F6). This reads the package's own source
// rather than a hand-kept list, so adding a ninth exported Err* without giving
// it a CodeFor arm fails here instead of shipping as a silent E_HUB_IO.
func TestSentinelSetIsClosedAtEight(t *testing.T) {
	want := []string{
		"ErrFenced",
		"ErrLeaseHeld",
		"ErrNotRegistered",
		"ErrOrder",
		"ErrRecipientRacing",
		"ErrRecipientStale",
		"ErrRecipientUnknown",
		"ErrRecipientUnreadable",
	}
	got := exportedErrVars(t)
	sort.Strings(got)
	if len(got) != len(want) {
		t.Fatalf("exported sentinels = %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("exported sentinels = %v, want %v", got, want)
		}
	}
}

func exportedErrVars(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return len(fi.Name()) < 8 || fi.Name()[len(fi.Name())-8:] != "_test.go"
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok || gen.Tok != token.VAR {
					continue
				}
				for _, spec := range gen.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for _, name := range vs.Names {
						if len(name.Name) > 3 && name.Name[:3] == "Err" {
							names = append(names, name.Name)
						}
					}
				}
			}
		}
	}
	return names
}
