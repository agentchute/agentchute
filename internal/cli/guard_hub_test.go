package cli

import (
	"strings"
	"testing"
	"time"
)

// PR #216 gate (codex P2): the hub rule matched text, so routine read-only
// commands that merely MENTION a hub invocation were denied while mail was
// held — and a checkout path containing "agentchute" was read as the binary.
// The rule now recognises an invocation only in command position.
func TestGuardHubRuleAllowsReadOnlyMentions(t *testing.T) {
	for _, cmd := range []string{
		"git log --grep hub",
		"git log --grep='agentchute hub join'",
		`rg -n 'agentchute hub authorize' internal/cli`,
		"git -C /Users/alex/code/agentchute log --grep='hub join'",
		"echo agentchute hub join",
		`grep -rn "ac hub authorize" docs/hub.md`,
		"agentchute status",
		"agentchute send --to codex --body 'please run agentchute hub join for me'",
		"cat /Users/alex/code/agentchute/docs/hub.md | grep 'hub join'",
	} {
		if guardCommandDenied("Bash " + cmd) {
			t.Errorf("guard denied a read-only mention: %s", cmd)
		}
	}
}

// Every real spelling still counts, including the ones the shell folds back
// into one invocation and the ones nested in another command.
func TestGuardHubRuleStillDeniesRealInvocations(t *testing.T) {
	for _, cmd := range []string{
		"agentchute hub authorize --agent claude-code --pool /p --key 'ssh-ed25519 AAAA'",
		"ac hub join ssh://alex@hub.example/home/alex/pool --as x",
		"${AGENTCHUTE_BIN:-agentchute} hub authorize --list",
		"${AGENTCHUTE_BIN} hub join ssh://h/p --as y",
		"$AGENTCHUTE_BIN hub join ssh://h/p --as y",
		"agentchute dispatch -- hub join ssh://h/p --name codex",
		"agentchute dispatch --shim-dir /x -- hub join ssh://h/p --name codex",
		"cd /tmp && agentchute  hub   join ssh://h/p --as y",
		"ac --as codex-tiny hub join ssh://h/p --name codex",
		"agentchute --control-repo /p hub authorize --agent x --pool /p --key k",
		"agentchute --as=x hub authorize --agent x --pool /p --key k",
		"agentchute 'hub' join ssh://h/p --as y",
		`agentchute hub "authorize" --list`,
		"agentchute hub \\\njoin ssh://h/p --as y",
		"agentchute \\\nhub join ssh://h/p --as y",
		`"$HOME/.local/bin/agentchute" hub join ssh://h/p --as y`,
		"/usr/local/bin/agentchute hub session --agent claude-code --pool /p --pool-id 0123456789ab",
		"agentchute hub${IFS}join ssh://h/p --as y",
		"agentchute hub$IFS'join' ssh://h/p --as y",
		`agentchute hub$'\t'authorize --list`,
		"FOO=1 agentchute hub join ssh://h/p --as y",
		"env -i HOME=/tmp agentchute hub join ssh://h/p --as y",
		"sudo -u alex agentchute hub authorize --list",
		"nohup agentchute hub join ssh://h/p --as y &",
		"timeout 5 agentchute hub join ssh://h/p --as y",
		"echo x | xargs agentchute hub join",
		"sh -c 'agentchute hub join ssh://h/p --as y'",
		`bash -lc "agentchute hub authorize --list"`,
		"eval 'agentchute hub join ssh://h/p --as y'",
		"echo $(agentchute hub join ssh://h/p --as y)",
		"echo `agentchute hub authorize --list`",
		"true; agentchute hub join ssh://h/p --as y",
		"ssh hub.example 'agentchute hub authorize --agent x --pool /p --key k'",
	} {
		if !guardCommandDenied("Bash " + cmd) {
			t.Errorf("guard allowed a real invocation: %q", cmd)
		}
	}
}

// Forms the background security review found the first parser missed: shell
// keywords and braces in command position, redirections before the program, a
// tool name the guard does not list, option clusters that only look like -c,
// wrapper options that take a value, any ${IFS...} expansion, and text piped
// into a shell that reads its commands from stdin.
func TestGuardHubRuleCoversKeywordsRedirectionsAndFedShells(t *testing.T) {
	for _, cmd := range []string{
		"if true; then agentchute hub join ssh://h/p --as y; fi",
		"for x in 1; do agentchute hub join ssh://h/p --as y; done",
		"{ agentchute hub join ssh://h/p --as y; }",
		"f(){ agentchute hub join ssh://h/p --as y; }; f",
		"! agentchute hub join ssh://h/p --as y",
		">/dev/null agentchute hub join ssh://h/p --as y",
		"2>&1 agentchute hub join ssh://h/p --as y",
		"agentchute hub join ssh://h/p --as y >/dev/null 2>&1",
		"bash --norc -c 'agentchute hub join ssh://h/p --as y'",
		"sh -ec 'agentchute hub join ssh://h/p --as y'",
		"timeout -s KILL 5 agentchute hub join ssh://h/p --as y",
		"xargs -I {} agentchute hub join {} < urls",
		"env -C /tmp agentchute hub join ssh://h/p --as y",
		"sudo -H agentchute hub authorize --list",
		"agentchute hub${IFS:0:1}join ssh://h/p --as y",
		"echo 'agentchute hub join ssh://h/p --as y' | sh",
		"bash <<< 'agentchute hub authorize --list'",
		"cat cmds.txt | bash -s",
	} {
		if !guardCommandDenied("Bash " + cmd) {
			t.Errorf("guard allowed %q", cmd)
		}
	}
	// A tool name the guard has never heard of still prefixes the command.
	if !guardCommandDenied("SomeFutureShellTool agentchute hub join ssh://h/p --as y") {
		t.Error("an unlisted tool-name prefix hid the invocation")
	}
}

// The hub rule's work is bounded: a command built from repeated wrapper,
// shell or ssh words must not make the hook run long enough to be killed
// (a killed hook lets the command through). Past its budget it denies.
func TestGuardHubRuleWorkIsBounded(t *testing.T) {
	for _, cmd := range []string{
		strings.Repeat("env ", 60) + "true",
		"env " + strings.Repeat("ssh h ", 60) + "true",
		"env " + strings.Repeat("sh -c ", 60) + "true",
		strings.Repeat("timeout 1 nice ", 40) + strings.Repeat("eval ", 40) + "true",
	} {
		done := make(chan struct{})
		go func() {
			guardCommandDenied("Bash " + cmd)
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("guard still running after 2s on %d-byte input %.40q...", len(cmd), cmd)
		}
	}
}
