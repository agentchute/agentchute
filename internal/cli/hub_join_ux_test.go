package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentchute/agentchute/internal/loop"
)

// A pointer write that changes nothing must say nothing.
//
// A migration writes the pointer and the join that follows it writes the same
// value again, so a recovery run announced "wrote pointer" twice for one
// pointer. The first write is real and still reported; the second is a no-op
// and now silent.
func TestHubJoinPointerIsSilentWhenNothingChanges(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git", "info"), 0o700); err != nil {
		t.Fatal(err)
	}
	url := "ssh://alex@hub.example/home/alex/pool"

	first, err := captureStdout(t, func() error { return writeHubJoinPointer(root, url) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first, "wrote pointer") {
		t.Fatalf("the FIRST write must still be reported: %q", first)
	}

	second, err := captureStdout(t, func() error { return writeHubJoinPointer(root, url) })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(second, "wrote pointer") || strings.Contains(second, "pointer replaced") {
		t.Fatalf("re-writing the same value announced a change that did not happen: %q", second)
	}

	changed, err := captureStdout(t, func() error { return writeHubJoinPointer(root, url+"-two") })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(changed, "pointer replaced") {
		t.Fatalf("a real change must still be reported: %q", changed)
	}
}

// The shadowed-copy warning fires when PATH resolves elsewhere, and stays quiet
// when it resolves to us. Both arms, because a warning that always fires is as
// useless as one that never does.
func TestHubJoinWarnsOnlyWhenAnotherBinaryShadowsThisOne(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	original := hubJoinLookPath
	t.Cleanup(func() { hubJoinLookPath = original })

	hubJoinLookPath = func() (string, error) { return self, nil }
	quiet, err := captureStdout(t, func() error { warnHubJoinShadowedBinary(); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(quiet, "warning") {
		t.Fatalf("warned although PATH resolves to this very binary: %q", quiet)
	}

	other := filepath.Join(t.TempDir(), "agentchute")
	if err := os.WriteFile(other, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	hubJoinLookPath = func() (string, error) { return other, nil }
	loud, err := captureStdout(t, func() error { warnHubJoinShadowedBinary(); return nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"warning", other, loop.PointerFileName, "ssh://"} {
		if !strings.Contains(loud, want) {
			t.Fatalf("shadow warning does not mention %q: %q", want, loud)
		}
	}

	hubJoinLookPath = func() (string, error) { return "", errors.New("not on PATH") }
	absent, err := captureStdout(t, func() error { warnHubJoinShadowedBinary(); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(absent, "warning") {
		t.Fatalf("warned although nothing is on PATH to shadow us: %q", absent)
	}
}

// The auto-authorize probe must bound its connect and must NOT stream ssh's
// stderr into the middle of agentchute's own sentence.
func TestHubJoinAutoAuthorizeBoundsConnectAndCapturesSSHOutput(t *testing.T) {
	remote, err := loop.ParseRemoteURL("ssh://alex@203.0.113.1/home/alex/pool")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	// A stand-in ssh that records its argv and writes the kind of chatter the
	// real one does.
	fake := filepath.Join(dir, "ssh")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + filepath.Join(dir, "argv") +
		"\necho 'Permission denied, please try again.' >&2\nexit 255\n"
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	out, err := captureStdout(t, func() error {
		return runHubJoinAutoAuthorize(remote, "codex", "ssh-ed25519 AAAA codex", false)
	})
	if err == nil {
		t.Fatal("probe reported success although ssh exited 255")
	}
	if strings.Contains(out, "Permission denied") {
		t.Fatalf("ssh's stderr was streamed through instead of captured: %q", out)
	}

	argv, readErr := os.ReadFile(filepath.Join(dir, "argv"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(argv), "ConnectTimeout=") {
		t.Fatalf("probe has no ConnectTimeout, so a black-holed hub hangs the join unbounded: %q", argv)
	}

	// And the transcript must survive on the error, for the caller to print
	// AFTER it finishes its sentence.
	var probe *sshProbeError
	if !errors.As(err, &probe) {
		t.Fatalf("error does not carry the ssh transcript: %v", err)
	}
	if !strings.Contains(probe.transcript, "Permission denied") {
		t.Fatalf("transcript = %q, want ssh's own words", probe.transcript)
	}
	printed, printErr := captureStdout(t, func() error { printSSHProbeTranscript(err); return nil })
	if printErr != nil {
		t.Fatal(printErr)
	}
	if !strings.Contains(printed, "ssh said:") || !strings.Contains(printed, "    Permission denied") {
		t.Fatalf("transcript is not replayed as an indented transcript: %q", printed)
	}
}
