package spectest

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// The local verification ritual must not be weaker than the gate that judges the
// push.
//
// `tools/test.sh` is what every lane is told to run before pushing; ci.yaml and
// release.yaml are what decide whether the push is acceptable. For an unknown
// stretch they disagreed: CI ran `go test -race ./...` and the ritual did not
// (#153). That is invisible by construction — a passing local run looks the same
// either way — and it cost a red, with a worse version available, since
// release.yaml runs -race too and can fail the release job AFTER the tag is
// pushed.
//
// #153 fixed the instance. This fixes the class: the two files are compared, so
// the next divergence fails here instead of on a runner, or after a tag.
//
// SCOPED TO `go test` INVOCATIONS deliberately. Comparing every step would make
// this a formatting test — CI's gofmt step is a multi-line shell script and the
// ritual's is `gofmt -w .`, the same intent expressed differently, and a check
// that demanded they match textually would be noise nobody could satisfy. Test
// invocations are where the flags live, so they are where a silent weakening
// hides.
func TestRitualRunsEveryTestCommandCIDoes(t *testing.T) {
	ritual := readRepoFile(t, "tools", "test.sh")
	for _, workflow := range []string{"ci.yaml", "release.yaml"} {
		t.Run(workflow, func(t *testing.T) {
			commands := goTestCommands(readRepoFile(t, ".github", "workflows", workflow))
			if len(commands) == 0 {
				t.Fatalf("no `go test` step found in %s — this check just stopped checking anything", workflow)
			}
			for _, command := range commands {
				if !ritualRuns(ritual, command) {
					t.Errorf("%s runs %q and tools/test.sh does not.\n"+
						"The ritual lanes are told to run before pushing is now weaker than the gate judging the push. "+
						"Add it to tools/test.sh (env-stripped, like the neighbouring lines), or remove it from the workflow.",
						workflow, command)
				}
			}
		})
	}
}

// goTestCommands returns the `go test …` command from every `run:` step in the
// workflow, normalised to the part that matters. A step may wrap it — CI's
// conformance step is `cd conformance && go test ./...` — so the prefix is kept,
// since running the same command in a different directory is a different check.
func goTestCommands(workflow string) []string {
	var found []string
	for _, line := range strings.Split(workflow, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "run:") {
			continue
		}
		command := strings.TrimSpace(strings.TrimPrefix(trimmed, "run:"))
		if !strings.Contains(command, "go test") {
			continue
		}
		found = append(found, command)
	}
	return found
}

// ritualStripEnvRE removes the ritual's `env $strip_env ` prefix, which exists
// only because a lane's leaked AGENTCHUTE_* vars would otherwise fail the run
// (AGENTS.md E10). CI needs no such prefix, so it is noise for this comparison —
// and stripping it is what lets the conformance step compare as one string
// rather than being special-cased.
var ritualStripEnvRE = regexp.MustCompile(`env \$strip_env `)

func ritualRuns(ritual, command string) bool {
	normalized := ritualStripEnvRE.ReplaceAllString(ritual, "")
	for _, line := range strings.Split(normalized, "\n") {
		trimmed := strings.TrimSpace(line)
		// Skip the `say` echo lines: they print the command as a label, so
		// matching them would let the ritual ANNOUNCE a step it never runs.
		if strings.HasPrefix(trimmed, "say ") || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(trimmed, command) {
			return true
		}
	}
	return false
}

// The `say` skip in ritualRuns is load-bearing, so it is asserted rather than
// described. Without it a ritual that ANNOUNCES a step it never runs satisfies
// the check — measured: deleting the real invocation while leaving its `say`
// label behind passes once the skip is removed. That is the precise failure this
// whole test exists to catch, so the exemption gets its own row.
func TestRitualAnnouncingAStepIsNotRunningIt(t *testing.T) {
	const command = "go test -race ./..."
	announcedOnly := "#!/bin/sh\nsay \"" + command + "\"\n"
	if ritualRuns(announcedOnly, command) {
		t.Fatal("a script that only echoes the command counts as running it; the check can be satisfied by a label")
	}
	// The control, or the row above would also pass for a script that contains
	// nothing at all.
	if !ritualRuns(announcedOnly+"env $strip_env "+command+" || exit 1\n", command) {
		t.Fatal("a real env-stripped invocation was not recognised")
	}
	// Comments are skipped for the same reason: #153's own change explains
	// -race in a comment block directly above the line that runs it.
	if ritualRuns("#!/bin/sh\n# "+command+" is deliberately not run here\n", command) {
		t.Fatal("a commented-out command counts as running it")
	}
}

func readRepoFile(t *testing.T, parts ...string) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate ritual_test.go")
	}
	path := filepath.Join(append([]string{filepath.Dir(self), "..", ".."}, parts...)...)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(fmt.Errorf("read %s: %w", path, err))
	}
	return string(data)
}
