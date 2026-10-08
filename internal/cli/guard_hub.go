package cli

import (
	"path/filepath"
	"strings"
)

// guard_hub.go — the guard's hub rule (review 2026-10-08, S10; PR #216 gate):
// while a lane holds claimed mail, deny `hub authorize`, `hub join` and
// `hub session` when they are RUN — not when they are mentioned. A text match
// denied `git log --grep='agentchute hub join'` and read a checkout path such
// as /Users/alex/code/agentchute as the binary; a match that only allowed
// adjacent words was spelled around with quotes, continuations, $IFS or global
// flags. So the command text is split into shell words and commands the way a
// shell would, and only a command whose program is agentchute/ac (after
// assignments, wrappers such as env/sudo/xargs, and the CLI's own global
// options) counts. Command text nested in $(...), backticks, sh -c, eval and
// an ssh remote command is examined the same way.
//
// Still a speed bump, like the rest of the guard: a binary reached through an
// arbitrary variable or alias is not seen. The S10 control is hub authorize's
// own terminal-gated refusal.

var guardHubSubcommands = map[string]bool{"authorize": true, "join": true, "session": true}

var guardShells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true}

// guardReserved are shell words that can stand in command position without
// being the program (if/then/do/{/! ...).
var guardReserved = map[string]bool{"if": true, "then": true, "else": true, "elif": true, "fi": true, "do": true, "done": true, "while": true, "until": true, "!": true, "{": true, "}": true, "esac": true}

// guardWrappers run another command given as later words.
var guardWrappers = map[string]bool{"env": true, "command": true, "exec": true, "nohup": true, "time": true, "nice": true, "timeout": true, "sudo": true, "doas": true, "xargs": true, "stdbuf": true, "builtin": true}

// guardHubInvocation reports whether lower-cased command text runs a guarded
// hub subcommand anywhere in command position.
func guardHubInvocation(text string) bool {
	budget := guardHubBudget
	return guardHubInvocationDepth(text, 0, &budget)
}

// guardHubBudget bounds the rule's work, counted in bytes lexed plus words
// examined. Nested text and wrappers multiply the work, and a hook killed for
// running long lets the command through, so past the budget the rule denies.
const guardHubBudget = 4 << 20

func guardHubInvocationDepth(text string, depth int, budget *int) bool {
	*budget -= len(text) + 1
	if depth > 8 || *budget < 0 {
		return true // pathological nesting or size: fail closed
	}
	commands, nested := guardShellCommands(text)
	for _, inner := range nested {
		if guardHubInvocationDepth(inner, depth+1, budget) {
			return true
		}
	}
	for _, words := range commands {
		if guardHubCommand(words, depth, budget) {
			return true
		}
	}
	return false
}

// guardHubCommand examines one simple command.
func guardHubCommand(words []string, depth int, budget *int) bool {
	for j := 0; j+1 < len(words); j++ {
		if filepath.Base(words[j]) == "env" && (words[j+1] == "-s" || strings.HasPrefix(words[j+1], "--split-string")) && j+2 < len(words) {
			if guardHubInvocationDepth(words[j+2], depth+1, budget) {
				return true
			}
		}
	}
	i := guardSkipPrefix(words)
	if i >= len(words) {
		return false
	}
	if guardWrappers[filepath.Base(words[i])] {
		// Which wrapper options take a value differs per wrapper, and the guard
		// sees lower-cased text, so it does not guess: every later word is
		// tried as the program, once each (a wrapper found there adds nothing,
		// since every word after it is already being tried).
		for j := i + 1; j < len(words); j++ {
			if guardHubProgramAt(words[j:], depth, budget) {
				return true
			}
		}
		return false
	}
	return guardHubProgramAt(words[i:], depth, budget)
}

// guardHubProgramAt examines words[0] as the program of a command.
func guardHubProgramAt(words []string, depth int, budget *int) bool {
	*budget -= len(words)
	if *budget < 0 {
		return true
	}
	i := 0
	prog := words[i]
	base := filepath.Base(prog)
	switch {
	case base == "agentchute" || base == "ac" || prog == "${agentchute_bin:-agentchute}" || prog == "${agentchute_bin}" || prog == "$agentchute_bin":
		return guardHubArgs(words[i+1:])
	case guardShells[base]:
		// -c (alone or in a short-option cluster such as -ec) runs its string;
		// with no -c and no script operand the shell reads its commands from
		// stdin — a pipe, a here-string, a file — which the guard cannot see, so
		// that fails closed.
		for j := i + 1; j < len(words); j++ {
			w := words[j]
			switch {
			case len(w) > 1 && w[0] == '-' && w[1] != '-' && strings.ContainsRune(w[1:], 'c'):
				if j+1 < len(words) {
					return guardHubInvocationDepth(words[j+1], depth+1, budget)
				}
				return true
			case w == "--rcfile" || w == "--init-file" || w == "-o" || w == "+o":
				j++
			case strings.HasPrefix(w, "-") || strings.HasPrefix(w, "+"):
			default:
				return false // a script operand: a file the guard cannot read
			}
		}
		return true
	case base == "eval":
		return guardHubInvocationDepth(strings.Join(words[i+1:], " "), depth+1, budget)
	case base == "ssh":
		if cmd := guardSSHRemoteCommand(words[i+1:]); cmd != "" {
			return guardHubInvocationDepth(cmd, depth+1, budget)
		}
	}
	return false
}

// guardHubArgs skips the CLI's global options (and a dispatch layer) and
// reports whether what follows is `hub <guarded subcommand>`.
func guardHubArgs(args []string) bool {
	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "dispatch":
			i++
		case a == "--":
			i++
		case a == "--shim-dir" || globalValueFlags[a]:
			i += 2
		case strings.HasPrefix(a, "-"):
			i++
		default:
			return a == "hub" && i+1 < len(args) && guardHubSubcommands[args[i+1]]
		}
	}
	return false
}

// guardSkipPrefix returns the index of the program word: past NAME=value
// assignments and past wrapper commands with their options.
func guardSkipPrefix(words []string) int {
	i := 0
	for i < len(words) && (guardIsAssignment(words[i]) || guardReserved[words[i]]) {
		i++
	}
	return i
}

func guardIsAssignment(w string) bool {
	eq := strings.IndexByte(w, '=')
	if eq <= 0 {
		return false
	}
	for _, c := range w[:eq] {
		if !(c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}

// guardSSHRemoteCommand returns the remote command an ssh invocation runs, or
// "" when it has none.
func guardSSHRemoteCommand(args []string) string {
	withValue := "bcDeEFIiJLlmOoPpQRSWw"
	i := 0
	for i < len(args) {
		a := args[i]
		if a == "--" {
			i++
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			break
		}
		if len(a) == 2 && strings.ContainsRune(withValue, rune(a[1])) {
			i += 2
			continue
		}
		i++
	}
	if i >= len(args) {
		return ""
	}
	return strings.Join(args[i+1:], " ") // past the destination
}

// guardShellCommands splits command text into simple commands (lists of words)
// the way a POSIX shell would for this purpose: quotes group, backslash
// escapes, a backslash-newline continues, ;, &, |, newline and parentheses
// separate commands, and $IFS / ${IFS} or an ANSI-C whitespace escape ($'\t')
// separate words. The text of every $(...) and `...` is returned in nested
// for the caller to examine as its own command line.
func guardShellCommands(s string) (commands [][]string, nested []string) {
	var words []string
	var word strings.Builder
	inWord := false
	endWord := func() {
		if inWord {
			words = append(words, word.String())
			word.Reset()
			inWord = false
		}
	}
	endCommand := func() {
		endWord()
		if len(words) > 0 {
			commands = append(commands, words)
			words = nil
		}
	}
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s) && s[i+1] == '\n':
			i += 2
		case c == '\\' && i+1 < len(s):
			word.WriteByte(s[i+1])
			inWord = true
			i += 2
		case c == ' ' || c == '\t' || c == '\r':
			endWord()
			i++
		case c == '>' || c == '<' || (c == '&' && i+1 < len(s) && s[i+1] == '>'):
			// A redirection: its fd number, operator and target are not words of
			// the command. A heredoc's body is data until its delimiter line.
			if inWord && guardAllDigits(word.String()) {
				word.Reset()
				inWord = false
			}
			endWord()
			j := i
			for j < len(s) && strings.IndexByte("<>&|-", s[j]) >= 0 {
				j++
			}
			op := s[i:j]
			for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
				j++
			}
			target, n := guardReadWord(s[j:])
			j += n
			if strings.HasPrefix(op, "<<") && !strings.HasPrefix(op, "<<<") && target != "" {
				j = guardSkipHeredoc(s, j, target)
			}
			i = j
		case c == '\n' || c == ';' || c == '&' || c == '|' || c == '(' || c == ')':
			endCommand()
			i++
		case c == '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				word.WriteString(s[i+1:])
				i = len(s)
			} else {
				word.WriteString(s[i+1 : i+1+j])
				i += j + 2
			}
			inWord = true
		case c == '"':
			i++
			for i < len(s) && s[i] != '"' {
				switch {
				case s[i] == '\\' && i+1 < len(s):
					word.WriteByte(s[i+1])
					i += 2
				case strings.HasPrefix(s[i:], "$("):
					inner, n := guardBalanced(s[i+2:])
					nested = append(nested, inner)
					i += 2 + n
				case s[i] == '`':
					j := strings.IndexByte(s[i+1:], '`')
					if j < 0 {
						j = len(s) - i - 1
					}
					nested = append(nested, s[i+1:i+1+j])
					i += j + 2
				default:
					word.WriteByte(s[i])
					i++
				}
			}
			i++
			inWord = true
		case strings.HasPrefix(s[i:], "$'"):
			j := i + 2
			var lit strings.Builder
			for j < len(s) && s[j] != '\'' {
				if s[j] == '\\' && j+1 < len(s) {
					switch s[j+1] {
					case 't', 'n', 'r', 'v':
						lit.WriteByte(' ')
					default:
						lit.WriteByte(s[j+1])
					}
					j += 2
					continue
				}
				lit.WriteByte(s[j])
				j++
			}
			i = j + 1
			if strings.TrimSpace(lit.String()) == "" {
				endWord()
			} else {
				word.WriteString(lit.String())
				inWord = true
			}
		case strings.HasPrefix(s[i:], "${ifs"):
			// Any expansion of IFS (${IFS}, ${IFS:0:1}, ...) splits words.
			endWord()
			if j := strings.IndexByte(s[i:], '}'); j >= 0 {
				i += j + 1
			} else {
				i = len(s)
			}
		case strings.HasPrefix(s[i:], "$ifs") && (i+4 == len(s) || !guardIsNameByte(s[i+4])):
			endWord()
			i += len("$ifs")
		case strings.HasPrefix(s[i:], "$("):
			inner, n := guardBalanced(s[i+2:])
			nested = append(nested, inner)
			i += 2 + n
			inWord = true
		case c == '`':
			j := strings.IndexByte(s[i+1:], '`')
			if j < 0 {
				j = len(s) - i - 1
			}
			nested = append(nested, s[i+1:i+1+j])
			i += j + 2
			inWord = true
		default:
			word.WriteByte(c)
			inWord = true
			i++
		}
	}
	endCommand()
	return commands, nested
}

// guardBalanced returns the text up to the ')' that closes an already-opened
// "$(" and how many bytes it consumed (including that ')').
func guardBalanced(s string) (string, int) {
	depth := 1
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\'':
			if j := strings.IndexByte(s[i+1:], '\''); j >= 0 {
				i += j + 1
			}
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return s[:i], i + 1
			}
		}
	}
	return s, len(s)
}

func guardIsNameByte(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func guardAllDigits(w string) bool {
	if w == "" {
		return false
	}
	for _, c := range w {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// guardReadWord reads one shell word (quotes removed) from the start of s.
func guardReadWord(s string) (string, int) {
	var b strings.Builder
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == ';' || c == '&' || c == '|' || c == '(' || c == ')' || c == '<' || c == '>':
			return b.String(), i
		case c == '\'' || c == '"':
			j := strings.IndexByte(s[i+1:], c)
			if j < 0 {
				b.WriteString(s[i+1:])
				return b.String(), len(s)
			}
			b.WriteString(s[i+1 : i+1+j])
			i += j + 2
		case c == '\\' && i+1 < len(s):
			b.WriteByte(s[i+1])
			i += 2
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), i
}

// guardSkipHeredoc returns the index just past a heredoc body that starts on
// the line after position i and ends at a line equal to delim (leading tabs
// allowed, as <<- permits).
func guardSkipHeredoc(s string, i int, delim string) int {
	nl := strings.IndexByte(s[i:], '\n')
	if nl < 0 {
		return len(s)
	}
	j := i + nl + 1
	for j < len(s) {
		end := strings.IndexByte(s[j:], '\n')
		line := s[j:]
		next := len(s)
		if end >= 0 {
			line = s[j : j+end]
			next = j + end + 1
		}
		if strings.TrimLeft(line, "\t") == delim {
			return next
		}
		j = next
	}
	return len(s)
}
