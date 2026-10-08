package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/agentchute/agentchute/internal/hubclient"
	"github.com/agentchute/agentchute/internal/hubwire"
	"github.com/agentchute/agentchute/internal/loop"
)

type hubJoinOptions struct {
	URL          string
	Name         string
	AgentID      string
	ResetHostKey bool
	RotateKey    bool
	Replace      bool
}

var hubJoinProbe = func(remote *loop.RemoteConfig, agentID, keyPath string) (hubwire.HelloOK, []string, error) {
	return hubclient.Probe(context.Background(), remote, agentID, version, keyPath)
}

// hubJoinPinnedProbe is the migration's same-hub proof (see
// hubclient.ProbeWithPinnedHostKey).
var hubJoinPinnedProbe = func(remote *loop.RemoteConfig, agentID, keyPath, pinnedKnownHosts string) (hubwire.HelloOK, []string, error) {
	return hubclient.ProbeWithPinnedHostKey(context.Background(), remote, agentID, version, keyPath, pinnedKnownHosts)
}

var hubJoinAutoAuthorize = runHubJoinAutoAuthorize
var hubJoinReapMux = hubclient.ReapSSHMux
var hubJoinInstallShims = installHubJoinShims
var hubJoinHostname = os.Hostname
var hubJoinFingerprint = readHubJoinFingerprint

// The known_hosts files a first join seeds from (seedHubKnownHosts). Vars so a
// row can point them at fixtures rather than this machine's real trust.
var (
	hubJoinUserKnownHosts = func() string {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		return filepath.Join(home, ".ssh", "known_hosts")
	}
	hubJoinSystemKnownHosts = "/etc/ssh/ssh_known_hosts"
)

// errHubJoinIncomplete is a VERDICT sentinel, not a failure: the local half of
// the join succeeded, and re-running after authorizing on the hub is the
// intended next step — but the machine is NOT yet joined, and exiting 0 told
// every script otherwise. It exits 2, the code this CLI already reserves for
// "this is a verdict, not a command failure" (#178).
var errHubJoinIncomplete = errors.New("hub join: authorization still pending on the hub; re-run after authorizing")

func cmdHubJoin(args []string) error {
	fs := flag.NewFlagSet("hub join", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var opts hubJoinOptions
	fs.StringVar(&opts.Name, "name", "", "local wrapper name")
	fs.StringVar(&opts.AgentID, "as", "", "pool-wide agent id")
	fs.BoolVar(&opts.ResetHostKey, "reset-hostkey", false, "replace the pinned host key after operator confirmation")
	fs.BoolVar(&opts.RotateKey, "rotate-key", false, "rotate this joined identity's key")
	fs.BoolVar(&opts.Replace, "replace", false, "repoint a checkout that already points at a DIFFERENT hub")
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		opts.URL = args[0]
		args = args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return hubJoinUsage(err)
	}
	if opts.URL == "" && fs.NArg() == 1 {
		opts.URL = fs.Arg(0)
	} else if fs.NArg() != 0 {
		return hubJoinUsage(fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " ")))
	}
	if opts.URL == "" {
		return hubJoinUsage(fmt.Errorf("missing hub URL"))
	}
	if (strings.TrimSpace(opts.Name) == "") == (strings.TrimSpace(opts.AgentID) == "") {
		return hubJoinUsage(fmt.Errorf("exactly one of --name or --as is required"))
	}
	remote, err := loop.ParseRemoteURL(opts.URL)
	if err != nil {
		return err
	}
	root, _, err := resolveInitRoot()
	if err != nil {
		return err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return err
	}
	// Learn which ids the first scope must hold BEFORE taking anything. A frozen
	// tree left by an interrupted move belongs to some OTHER hub id, and the
	// sweep below deletes it — so that id has to be held too. Reading the marker
	// inside a lock on the new id and then taking the old one would invert the
	// sorted order the migration itself uses.
	frozenOrigin, err := hubFrozenOrigin(remote)
	if err != nil {
		return err
	}
	firstScope := []string{remote.HubID}
	if frozenOrigin != "" {
		firstScope = append(firstScope, frozenOrigin)
	}
	var oldHubID string
	completed := false
	err = withHubLocks(firstScope, func() error {
		// The pre-lock read chose WHICH locks to take, so it is a hint until it is
		// confirmed under them. Narrow but real: two joins to the same new URL,
		// the first crashing after its freeze — the second sees no frozen tree
		// before the lock, waits on the new id, and would otherwise sweep a tree
		// whose origin it never learned and whose hub it does not hold.
		//
		// Same discipline scope 2 already applies to the migration candidate.
		confirmed, originErr := hubFrozenOrigin(remote)
		if originErr != nil {
			return originErr
		}
		if confirmed != frozenOrigin {
			return fmt.Errorf("hub join: a frozen hub tree appeared or changed while locks were acquired (%q -> %q); nothing was touched, re-run", frozenOrigin, confirmed)
		}
		// Now finish a migration that was interrupted after it froze the old tree.
		// Nothing else can see that state — the old directory is gone, so there is
		// no candidate to find.
		if sweepErr := sweepFrozenHubMigration(remote); sweepErr != nil {
			return sweepErr
		}
		candidate, findErr := findHubMigrationCandidate(remote, opts)
		if findErr != nil {
			return findErr
		}
		if candidate == "" {
			completed = true
			return runHubJoin(root, remote, opts)
		}
		oldHubID = candidate
		return nil
	})
	if err != nil || completed {
		return err
	}
	return withHubLocks([]string{oldHubID, remote.HubID}, func() error {
		candidate, findErr := findHubMigrationCandidate(remote, opts)
		if findErr != nil {
			return findErr
		}
		if candidate != oldHubID {
			return fmt.Errorf("hub join: same-hub migration state changed while locks were acquired; re-run")
		}
		if err := migrateHubJoinState(root, oldHubID, remote); err != nil {
			return err
		}
		// The migration was proven: the new URL's host holds a key pinned for
		// the old hub. The moved known_hosts names only the OLD host, so make
		// that proof the new name's pin, or its first connection is accept-new.
		if err := hubclient.CarryHostKeyPin(filepath.Join(remote.HubDir, "known_hosts"), hubKnownHostsSpec(remote)); err != nil {
			return err
		}
		return runHubJoin(root, remote, opts)
	})
}

func runHubJoin(root string, remote *loop.RemoteConfig, opts hubJoinOptions) error {
	if err := refuseHubJoinRepoint(root, remote, opts); err != nil {
		return err
	}
	if opts.ResetHostKey {
		if err := os.Remove(filepath.Join(remote.HubDir, "known_hosts")); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	// Not after --reset-hostkey: it exists to accept the hub's NEW key after a
	// confirmed rebuild, and seeding would re-pin the old one from
	// ~/.ssh/known_hosts.
	if !opts.ResetHostKey {
		if err := seedHubKnownHosts(remote); err != nil {
			return err
		}
	}
	existing, err := hubclient.ReadHubConfig(remote.HubID)
	if err != nil && !errors.Is(err, hubclient.ErrHubConfigNotFound) {
		return err
	}
	if existing == nil {
		existing = &hubclient.HubConfig{URL: remote.URL, Names: map[string]string{}}
	}
	if existing.Names == nil {
		existing.Names = map[string]string{}
	}
	if envID := strings.TrimSpace(os.Getenv("AGENTCHUTE_AGENT_ID")); envID != "" {
		if mapped, ok := existing.Names[envID]; ok && mapped != envID {
			fmt.Fprintln(os.Stderr, hubLocalNameWarning(envID, mapped))
		}
	}
	agentID, localName, err := resolveHubJoinIdentity(existing, opts)
	if err != nil {
		return err
	}
	if err := loop.ValidateAgentID(agentID); err != nil {
		return err
	}
	if err := validateHubJoinShadowing(existing, localName, agentID, opts); err != nil {
		return err
	}

	state, generated, err := prepareHubKey(remote.HubDir, agentID)
	if err != nil {
		return err
	}
	if generated {
		fmt.Printf("generated key: %s\n", displayHomePath(state.Active.Private))
	}
	if opts.RotateKey && state.Staged == nil {
		staged, err := mintStagedHubKey(remote.HubDir, agentID, state)
		if err != nil {
			return err
		}
		state.Versions = append(state.Versions, staged)
		state.Staged = &staged
	}

	hello, incomplete, err := finishHubJoinKey(remote, agentID, &state)
	if err != nil {
		return err
	}
	// Recorded from the per-hub known_hosts, i.e. from the key this join's
	// authenticated connection verified — never from ssh-keyscan, which shows a
	// key without proving anyone holds it (review 2026-10-08, S11; the #164
	// fallback). Migration no longer needs this value to prove a hub: it pins
	// the old hub's known_hosts directly (findHubMigrationCandidate).
	fingerprint, readErr := hubJoinFingerprint(remote)
	if fingerprint == "" {
		warnHubJoinNoFingerprint(remote, readErr)
	}
	if fingerprint != "" {
		fmt.Printf("hub key recorded: %s\n", fingerprint)
	}
	if incomplete {
		updateHubJoinConfig(existing, remote, agentID, localName, remote.PoolPath, "", fingerprint)
		if err := hubclient.WriteHubConfig(remote.HubID, existing); err != nil {
			return err
		}
		if err := writeHubJoinPointer(root, remote.URL); err != nil {
			return err
		}
		warnHubJoinEnv(remote.URL)
		// #178: this join is NOT complete — authorization is still pending on
		// the hub — and it used to exit 0, so a script checking $? read
		// "half-joined" as "joined". The local half really did succeed (the
		// pointer and the hub key are recorded, and re-running is the intended
		// next step), so this is a verdict rather than a failure, which is
		// exactly what exit 2 already means here.
		return errHubJoinIncomplete
	}
	if !hello.Writable {
		return fmt.Errorf("joined but the hub session cannot WRITE the pool: check that user %q owns %s (ls -ld), that the .agentchute/loop tree is writable by it, and that the pool was not authorized against another user's checkout", remote.User, hello.Pool)
	}
	if existing.Pool12 != "" && existing.Pool12 != hello.Pool12 {
		return fmt.Errorf("hub: joined pool id %s, but the hub now reports %s", existing.Pool12, hello.Pool12)
	}
	updateHubJoinConfig(existing, remote, agentID, localName, hello.Pool, hello.Pool12, fingerprint)
	if err := hubclient.WriteHubConfig(remote.HubID, existing); err != nil {
		return err
	}
	if err := writeHubJoinPointer(root, remote.URL); err != nil {
		return err
	}
	if err := hubJoinInstallShims(); err != nil {
		return err
	}
	warnHubJoinEnv(remote.URL)
	fmt.Printf("joined: %s", agentID)
	if localName != "" {
		fmt.Printf(" (local name: %s)", localName)
	}
	fmt.Printf(" @ %s (agentchute-hub v%d)\n", remote.URL, hello.V)
	return nil
}

func resolveHubJoinIdentity(cfg *hubclient.HubConfig, opts hubJoinOptions) (agentID, localName string, err error) {
	if strings.TrimSpace(opts.AgentID) != "" {
		return strings.TrimSpace(opts.AgentID), "", nil
	}
	raw := strings.TrimSpace(opts.Name)
	spec, ok := wrapperForToken(raw)
	if !ok {
		return "", "", hubJoinNameError(raw)
	}
	localName = spec.AgentID
	if mapped := cfg.Names[localName]; mapped != "" {
		return mapped, localName, nil
	}
	host, err := hubJoinHostname()
	if err != nil {
		return "", "", err
	}
	host = strings.SplitN(host, ".", 2)[0]
	localPart := sanitizeHubJoinIDComponent(spec.AgentID)
	hostPart := sanitizeHubJoinIDComponent(host)
	if localPart == "" || hostPart == "" {
		return "", "", fmt.Errorf("hub join: --name and hostname must contain at least one letter or digit after sanitization")
	}
	return localPart + "-" + hostPart, localName, nil
}

func sanitizeHubJoinIDComponent(raw string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(raw) {
		valid := r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
		if valid {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash && b.Len() > 0 {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func validateHubJoinShadowing(cfg *hubclient.HubConfig, localName, agentID string, opts hubJoinOptions) error {
	if strings.TrimSpace(opts.AgentID) != "" {
		if mapped, ok := cfg.Names[agentID]; ok && mapped != agentID {
			return fmt.Errorf("hub join: %q is already this machine's local name for %q — a pool id %q would be unselectable here (runtime --as %s resolves to %s). Pick a different --as, or use the existing %s lane", agentID, mapped, agentID, agentID, mapped, agentID)
		}
		return nil
	}
	for _, joined := range cfg.JoinedAs {
		if joined == localName && joined != agentID {
			return fmt.Errorf("hub join: this machine already joined as pool id %q; recording %q as a local name for %q would shadow it (runtime --as %s would stop selecting it). Pick a different --name, or --as an explicit id", joined, localName, agentID, localName)
		}
	}
	return nil
}

func finishHubJoinKey(remote *loop.RemoteConfig, agentID string, state *hubKeyState) (hubwire.HelloOK, bool, error) {
	if state.Staged != nil {
		stagedHello, warnings, stagedErr := probeHubJoinKey(remote, agentID, state.Staged.Private)
		printHubJoinWarnings(warnings)
		if stagedErr == nil {
			if err := promoteHubKey(remote.HubDir, agentID, *state.Staged); err != nil {
				return hubwire.HelloOK{}, false, err
			}
			state.Active = *state.Staged
			if err := pruneOlderHubKeys(*state); err != nil {
				return hubwire.HelloOK{}, false, err
			}
			return stagedHello, false, nil
		}
		activeHello, activeWarnings, activeErr := probeHubJoinKey(remote, agentID, state.Active.Private)
		printHubJoinWarnings(activeWarnings)
		if activeErr != nil {
			if hubclient.ErrorCode(stagedErr) == "E_UNAUTHORIZED" && hubclient.ErrorCode(activeErr) == "E_UNAUTHORIZED" {
				if err := printHubAuthorizePaste(remote, agentID, *state.Staged, true); err != nil {
					return hubwire.HelloOK{}, false, err
				}
				return hubwire.HelloOK{}, true, nil
			}
			return hubwire.HelloOK{}, false, activeErr
		}
		_ = activeHello
		if hubclient.ErrorCode(stagedErr) != "E_UNAUTHORIZED" {
			return hubwire.HelloOK{}, false, stagedErr
		}
		ok, err := authorizeHubJoinKey(remote, agentID, *state.Staged, true)
		if err != nil {
			return hubwire.HelloOK{}, false, err
		}
		if !ok {
			return hubwire.HelloOK{}, true, nil
		}
		if err := promoteHubKey(remote.HubDir, agentID, *state.Staged); err != nil {
			return hubwire.HelloOK{}, false, err
		}
		state.Active = *state.Staged
		hello, warnings, err := probeHubJoinKey(remote, agentID, state.Active.Private)
		printHubJoinWarnings(warnings)
		if err != nil {
			return hubwire.HelloOK{}, false, err
		}
		if err := pruneOlderHubKeys(*state); err != nil {
			return hubwire.HelloOK{}, false, err
		}
		return hello, false, nil
	}

	hello, warnings, err := probeHubJoinKey(remote, agentID, state.Active.Private)
	printHubJoinWarnings(warnings)
	if err != nil && hubclient.ErrorCode(err) != "E_UNAUTHORIZED" {
		return hubwire.HelloOK{}, false, err
	}
	if hubclient.ErrorCode(err) == "E_UNAUTHORIZED" {
		replace := len(state.Versions) > 1
		ok, authErr := authorizeHubJoinKey(remote, agentID, state.Active, replace)
		if authErr != nil {
			return hubwire.HelloOK{}, false, authErr
		}
		if !ok {
			return hubwire.HelloOK{}, true, nil
		}
		hello, warnings, err = probeHubJoinKey(remote, agentID, state.Active.Private)
		printHubJoinWarnings(warnings)
		if err != nil {
			return hubwire.HelloOK{}, false, err
		}
	}
	if len(state.Versions) > 1 {
		if err := pruneOlderHubKeys(*state); err != nil {
			return hubwire.HelloOK{}, false, err
		}
	}
	return hello, false, nil
}

func probeHubJoinKey(remote *loop.RemoteConfig, agentID, keyPath string) (hubwire.HelloOK, []string, error) {
	hello, warnings, err := hubJoinProbe(remote, agentID, keyPath)
	if err == nil && hello.Agent != agentID {
		return hubwire.HelloOK{}, warnings, &hubclient.Error{Code: "E_IDENTITY", Msg: hubIdentityError(hello.Agent, agentID).Error()}
	}
	return hello, warnings, err
}

func authorizeHubJoinKey(remote *loop.RemoteConfig, agentID string, key hubKeyVersion, replace bool) (bool, error) {
	pubkey, err := readHubPublicKey(key)
	if err != nil {
		return false, err
	}
	fmt.Print("authorizing via your own SSH access… ")
	if err := hubJoinAutoAuthorize(remote, agentID, pubkey, replace); err != nil {
		fmt.Println("not available")
		printSSHProbeTranscript(err)
		fmt.Println(hubAuthorizePaste(remote, agentID, pubkey, replace))
		return false, nil
	}
	fmt.Println("ok")
	if replace {
		if err := hubJoinReapMux(remote, agentID, key.Private, remote.HubDir); err != nil {
			return false, fmt.Errorf("hub join: authorization changed, but the local SSH master could not be reaped; stop this lane before retrying: %w", err)
		}
	}
	return true, nil
}

func printHubAuthorizePaste(remote *loop.RemoteConfig, agentID string, key hubKeyVersion, replace bool) error {
	pubkey, err := readHubPublicKey(key)
	if err != nil {
		return err
	}
	fmt.Println(hubAuthorizePaste(remote, agentID, pubkey, replace))
	return nil
}

func hubAuthorizePaste(remote *loop.RemoteConfig, agentID, pubkey string, replace bool) string {
	return "Run this ON THE HUB, then retry here:\n  " + hubclient.HubAuthorizeCommand(agentID, remote.PoolPath, pubkey, replace)
}

// hubAutoAuthorizeTimeout bounds the auto-authorize probe. Every other ssh this
// package runs goes through BuildSSHInvocation, which sets ConnectTimeout=5;
// this one used to set nothing at all, so a hub that black-holes packets hung
// the join with no bound.
const hubAutoAuthorizeTimeout = "5"

func runHubJoinAutoAuthorize(remote *loop.RemoteConfig, agentID, pubkey string, replace bool) error {
	values := []string{"agentchute", "hub", "authorize", "--agent", agentID, "--pool", remote.PoolPath, "--key", pubkey}
	if replace {
		values = append(values, "--replace-key")
	}
	quoted := make([]string, len(values))
	for i, value := range values {
		if strings.ContainsRune(value, '\'') {
			return fmt.Errorf("hub authorize argument contains a single quote")
		}
		quoted[i] = "'" + value + "'"
	}
	cmd := exec.Command("ssh", hubAutoAuthorizeSSHArgs(remote, strings.Join(quoted, " "))...)
	// stdin stays attached: this path deliberately uses the operator's OWN ssh
	// access, so ssh may need to prompt. IdentitiesOnly is deliberately NOT set
	// here for the same reason — pinning an identity would defeat the point.
	//
	// stderr is CAPTURED rather than wired straight through. The caller prints
	// "authorizing via your own SSH access… " with no newline and completes the
	// sentence with the outcome; streaming the child's stderr put ssh's own
	// chatter between those two halves, so an operator read
	//     authorizing via your own SSH access… Permission denied, please try again.
	//     Received disconnect from ... Too many authentication failures
	//     Disconnected from ...
	//     not available
	// and took "please try again" for an instruction. It is not one; the real
	// instruction is four lines further down.
	var stderr bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, &stderr
	if err := cmd.Run(); err != nil {
		return &sshProbeError{err: err, transcript: stderr.String()}
	}
	return nil
}

// hubAutoAuthorizeSSHArgs is the auto-authorize ssh argv. This ssh uses the
// operator's OWN access (no pinned identity), which is the one path that can
// land on an unrestricted login — so agent and X11 forwarding are switched off
// explicitly whatever the user's ssh_config says (review 2026-10-08, S11).
func hubAutoAuthorizeSSHArgs(remote *loop.RemoteConfig, remoteCommand string) []string {
	args := []string{"-o", "ConnectTimeout=" + hubAutoAuthorizeTimeout, "-o", "ForwardAgent=no", "-o", "ForwardX11=no"}
	if remote.Port != 22 {
		args = append(args, "-p", strconv.Itoa(remote.Port))
	}
	return append(args, remote.Destination(), remoteCommand)
}

// sshProbeError carries what ssh said so the CALLER can print it once its own
// sentence is finished. Printing it here would only move the interleaving, not
// remove it: the caller writes "authorizing via your own SSH access… " with no
// newline and completes it with the outcome.
type sshProbeError struct {
	err        error
	transcript string
}

func (e *sshProbeError) Error() string { return e.err.Error() }
func (e *sshProbeError) Unwrap() error { return e.err }

// printSSHProbeTranscript replays a failed probe's ssh output indented, so it
// reads as a transcript rather than as agentchute talking. Nothing is printed
// when ssh said nothing.
func printSSHProbeTranscript(err error) {
	var probe *sshProbeError
	if !errors.As(err, &probe) {
		return
	}
	out := strings.TrimRight(probe.transcript, "\n")
	if strings.TrimSpace(out) == "" {
		return
	}
	fmt.Println("  ssh said:")
	for _, line := range strings.Split(out, "\n") {
		fmt.Printf("    %s\n", line)
	}
}

func updateHubJoinConfig(cfg *hubclient.HubConfig, remote *loop.RemoteConfig, agentID, localName, pool, pool12, fingerprint string) {
	cfg.URL = remote.URL
	cfg.Pool = pool
	cfg.Pool12 = pool12
	if fingerprint != "" {
		cfg.HostKeyFingerprint = fingerprint
	}
	if !containsString(cfg.JoinedAs, agentID) {
		cfg.JoinedAs = append(cfg.JoinedAs, agentID)
		sort.Strings(cfg.JoinedAs)
	}
	if cfg.Names == nil {
		cfg.Names = map[string]string{}
	}
	if localName != "" {
		cfg.Names[localName] = agentID
	}
}

// refuseHubJoinRepoint stops a join from silently repointing a checkout that
// already points at a DIFFERENT hub: every lane in it would follow the new
// pointer to a hub of the caller's choosing (review 2026-10-08, S11). It runs
// before any key is minted or any connection made. A same-hub migration has
// already rewritten the pointer to this URL by the time it gets here, and a
// local (non-ssh) pointer is not a hub, so neither needs --replace.
func refuseHubJoinRepoint(root string, remote *loop.RemoteConfig, opts hubJoinOptions) error {
	if opts.Replace {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(root, loop.PointerFileName))
	if err != nil {
		return nil
	}
	old, _ := loop.ParsePointerFile(string(data))
	if !strings.HasPrefix(strings.TrimSpace(old), "ssh://") {
		return nil
	}
	oldRemote, err := loop.ParseRemoteURL(old)
	if err == nil && oldRemote.HubID == remote.HubID {
		return nil
	}
	return fmt.Errorf("hub join: this checkout already points at another hub (%s); joining %s would repoint every lane here. If that is intended, re-run with --replace", strings.TrimSpace(old), remote.URL)
}

func writeHubJoinPointer(root, url string) error {
	path := filepath.Join(root, loop.PointerFileName)
	old := ""
	if data, err := os.ReadFile(path); err == nil {
		old, _ = loop.ParsePointerFile(string(data))
	}
	tmp, err := os.CreateTemp(root, ".agentchute-control-repo.tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := io.WriteString(tmp, url+"\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	tracked := exec.Command("git", "-C", root, "ls-files", "--error-unmatch", "--", loop.PointerFileName).Run() == nil
	if tracked {
		fmt.Printf("warning: %s is tracked; remove it from Git so this machine-local hub pointer cannot be committed\n", loop.PointerFileName)
	} else if err := appendHubPointerExclude(root); err != nil {
		return err
	}
	// Gated on a real change, and NOT hoisted above this switch. Unconditional, it
	// fired on exactly the no-op writes the switch below exists to silence — so a
	// migration-plus-join reported twice again, one line above its own fix. Its own
	// doc is the rule: warn at the moment the pointer changes MEANING.
	if old != url {
		warnHubJoinShadowedBinary()
	}
	switch {
	case old == url:
		// Nothing changed, so say nothing. A migration writes the pointer and then the
		// join that follows it writes the same value again; announcing both made a
		// recovery run report "wrote pointer" twice for one pointer.
	case old != "":
		fmt.Printf("pointer replaced: %s -> %s\n", old, url)
	default:
		fmt.Printf("wrote pointer: %s (ignored via .git/info/exclude)\n", loop.PointerFileName)
	}
	return nil
}

// hubJoinLookPath is the PATH resolution used by the shadowed-copy warning. It
// is a package var so the row can drive both arms without touching PATH.
var hubJoinLookPath = func() (string, error) { return exec.LookPath("agentchute") }

// warnHubJoinShadowedBinary warns when the agentchute PATH resolves to is not
// the one performing the join.
//
// The pointer this join just wrote holds an `ssh://` URL, and an agentchute
// old enough to predate hub support does not recognise those: it reads the URL
// as a relative PATH and fails with a mangled lstat, e.g.
//
//	lstat /home/dmin/checkout/ssh:/alex@host/home/alex/pool: no such file or directory
//
// which says nothing about needing a newer binary. Two copies on one machine is
// the ordinary state mid-upgrade, and the old copy's message cannot be fixed
// from here — so the warning has to come from the side that still can, at the
// moment the pointer changes meaning. Resolution only; the other binary is
// never executed.
func warnHubJoinShadowedBinary() {
	resolved, err := hubJoinLookPath()
	if err != nil {
		return
	}
	self, err := os.Executable()
	if err != nil {
		return
	}
	if selfResolved, err := filepath.EvalSymlinks(self); err == nil {
		self = selfResolved
	}
	if resolvedReal, err := filepath.EvalSymlinks(resolved); err == nil {
		resolved = resolvedReal
	}
	if resolved == self {
		return
	}
	fmt.Printf("warning: `agentchute` on PATH resolves to %s, not the %s running this join\n", resolved, self)
	fmt.Printf("  %s now holds an ssh:// hub URL. A copy that predates hub support reads that as a file path and fails with a confusing lstat error; upgrade or reorder the other copy.\n", loop.PointerFileName)
}

// appendHubPointerExclude keeps the control-repo pointer out of git's way.
//
// A git failure used to return nil, which is right for the common case — this
// is not a git repository, so there is nothing to exclude — and silent for
// every other case. Those are different: an unreadable git dir, a git that is
// not installed, or a repository this user cannot read all end with the pointer
// file NOT excluded, which surfaces much later as an untracked file the
// operator did not create and may commit.
//
// Not being a repository stays silent. Anything else says so, and does not fail
// the join: the pointer is written either way, and refusing to join over a
// gitignore nicety would be a worse trade than the one being fixed.
func appendHubPointerExclude(root string) error {
	cmd := exec.Command("git", "-C", root, "rev-parse", "--git-path", "info/exclude")
	out, err := cmd.Output()
	if err != nil {
		if !gitSaysNotARepository(err) {
			fmt.Fprintf(os.Stderr, "warning: could not ask git where to exclude %s (%v); the pointer file may show up as untracked\n", loop.PointerFileName, gitFailureDetail(err))
		}
		return nil
	}
	path := strings.TrimSpace(string(out))
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == loop.PointerFileName {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintln(f, loop.PointerFileName)
	return err
}

// gitSaysNotARepository reports the one failure that means "nothing to do here".
// git writes it to stderr, which exec.Cmd.Output captures into ExitError.Stderr.
func gitSaysNotARepository(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	return strings.Contains(strings.ToLower(string(exitErr.Stderr)), "not a git repository")
}

// gitFailureDetail prefers what git said over the exit status, which on its own
// tells an operator nothing they can act on.
func gitFailureDetail(err error) string {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if said := strings.TrimSpace(string(exitErr.Stderr)); said != "" {
			return said
		}
	}
	return err.Error()
}

func installHubJoinShims() error {
	dir, err := defaultSetupShimDir()
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return err
	}
	if err := installDispatcher(dir, exe, true); err != nil {
		return err
	}
	if _, err := removeLegacyWrapperShims(dir); err != nil {
		return err
	}
	// setupEnsureShimPath below says this itself, once, and says it AFTER it has
	// tried to fix PATH. Warning here as well printed the same advice twice
	// before either attempt had happened.
	return setupEnsureShimPath(setupOptions{ShimDir: dir})
}

// warnHubJoinNoFingerprint is the other half of #164: the read error used to be
// discarded outright, so a hub that could never be migrated was recorded in
// silence.
//
// It warns rather than refusing. By this point the join has authenticated,
// written its key and been accepted; stranding a working machine over a
// diagnostic value would be a worse outcome than the one being fixed. What it
// must not do is stay quiet, and it must say what actually breaks — an operator
// has no reason to care about a fingerprint field, and the symptom shows up much
// later wearing someone else's remedy.
func warnHubJoinNoFingerprint(remote *loop.RemoteConfig, readErr error) {
	fmt.Fprintln(os.Stderr, "warning: joined, but this hub's host-key fingerprint could not be recorded.")
	if readErr != nil {
		fmt.Fprintf(os.Stderr, "  %s: %v\n", displayHomePath(filepath.Join(remote.HubDir, "known_hosts")), readErr)
	}
	fmt.Fprintln(os.Stderr, "  The join itself is complete; what this costs you is later. Migrating this hub to a new URL proves the two are the same hub with the host key pinned in that known_hosts file, and with none pinned the move is never offered: a `hub join` at the new URL is treated as a FRESH join and refused for having an authorized key already. Re-run the same `hub join` command once the hub is reachable and this records itself.")
}

func readHubJoinFingerprint(remote *loop.RemoteConfig) (string, error) {
	knownHosts := filepath.Join(remote.HubDir, "known_hosts")
	out, err := exec.Command("ssh-keygen", "-lf", knownHosts).Output()
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		return "", fmt.Errorf("could not parse host-key fingerprint")
	}
	return fields[1], nil
}

// seedHubKnownHosts copies this machine's existing trust for the hub host into
// the per-hub known_hosts before the FIRST connection. That file is passed as
// ssh's UserKnownHostsFile, which replaces ~/.ssh/known_hosts: a hub key the
// user had already verified was never consulted, and accept-new trusted
// whatever key answered first (review 2026-10-08, S11). Only when the per-hub
// file does not exist — once it does, it is the pin, and a seeded copy of a
// later ~/.ssh change must not silently re-pin it.
//
// Listing ~/.ssh/known_hosts beside the per-hub file in UserKnownHostsFile was
// the other option, and it is weaker: a key the user later accepted with plain
// ssh would then pass agentchute's pin too, and the fingerprint join records
// would come from whichever file happened to match.
// hubKnownHostsSpec is the name ssh checks this hub's host key under. The hub
// connection honours the user's ssh_config, so that is a HostKeyAlias when one
// is configured, and otherwise the host with the EFFECTIVE port — which a
// configured Port can change (PR #216 gate, codex P2). ssh -G resolves both the
// way the connection will. If it cannot, the fallback is loud, never silent.
func hubKnownHostsSpec(remote *loop.RemoteConfig) string {
	alias, port, err := hubJoinResolveSSH(remote)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not resolve %s through your ssh config (ssh -G: %v); looking up its host key as %s:%d\n", remote.Host, err, remote.Host, remote.Port)
		alias, port = "", remote.Port
	}
	if alias != "" {
		return alias
	}
	if port != 22 {
		return fmt.Sprintf("[%s]:%d", remote.Host, port)
	}
	return remote.Host
}

// hubJoinSSHConfig, when set, is passed to ssh -G as -F. Production leaves it
// empty so ssh reads the same config the hub connection does (ssh finds it
// through the account's passwd home, not $HOME — which is why a row needs it).
var hubJoinSSHConfig = ""

// hubJoinResolveSSH asks ssh for the effective HostKeyAlias and port of the
// hub destination.
var hubJoinResolveSSH = func(remote *loop.RemoteConfig) (alias string, port int, err error) {
	args := []string{"-G"}
	if hubJoinSSHConfig != "" {
		args = append(args, "-F", hubJoinSSHConfig)
	}
	if remote.Port != 22 {
		args = append(args, "-p", strconv.Itoa(remote.Port))
	}
	out, err := exec.Command("ssh", append(args, remote.Destination())...).Output()
	if err != nil {
		return "", 0, err
	}
	port = remote.Port
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		switch strings.ToLower(fields[0]) {
		case "hostkeyalias":
			alias = fields[1]
		case "port":
			if p, perr := strconv.Atoi(fields[1]); perr == nil {
				port = p
			}
		}
	}
	return alias, port, nil
}

func seedHubKnownHosts(remote *loop.RemoteConfig) error {
	dest := filepath.Join(remote.HubDir, "known_hosts")
	if _, err := os.Lstat(dest); err == nil || !os.IsNotExist(err) {
		return nil
	}
	spec := hubKnownHostsSpec(remote)
	var lines []string
	for _, src := range []string{hubJoinUserKnownHosts(), hubJoinSystemKnownHosts} {
		if src == "" {
			continue
		}
		if _, err := os.Stat(src); err != nil {
			continue
		}
		// ssh-keygen -F matches hashed entries too, and exits 1 on no match.
		out, err := exec.Command("ssh-keygen", "-F", spec, "-f", src).Output()
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(out), "\n") {
			if trimmed := strings.TrimSpace(line); trimmed != "" && !strings.HasPrefix(trimmed, "#") {
				lines = append(lines, trimmed)
			}
		}
	}
	if len(lines) == 0 {
		return nil
	}
	if err := os.MkdirAll(remote.HubDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(dest, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
}

func warnHubJoinEnv(url string) {
	raw := strings.TrimSpace(os.Getenv("AGENTCHUTE_CONTROL_REPO"))
	if raw == "" {
		return
	}
	remote, err := loop.ParseRemoteURL(raw)
	if err != nil || remote.URL != url {
		fmt.Fprintf(os.Stderr, "warning: AGENTCHUTE_CONTROL_REPO=%q overrides the new pointer %s; unset it before using this checkout\n", raw, url)
	}
}

func printHubJoinWarnings(warnings []string) {
	for _, warning := range warnings {
		fmt.Fprintf(os.Stderr, "warning: %s\n", warning)
	}
}

func displayHomePath(path string) string {
	home, err := os.UserHomeDir()
	if err == nil {
		if rel, err := filepath.Rel(home, path); err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return filepath.Join("~", rel)
		}
	}
	return path
}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func hubJoinUsage(err error) error {
	return fmt.Errorf("%w\nusage: agentchute hub join ssh://[user@]host[:port]/abs/path/to/pool (--name <local-name> | --as <agent-id>)\n  --name mints the pool id <local-name>-<hostname> (e.g. --name codex on host tiny -> codex-tiny) and must be a known wrapper token; --as uses your id verbatim.\n  The path is the pool's absolute path ON THE HUB (run `pwd` there).", err)
}
