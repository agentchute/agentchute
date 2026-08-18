package cli

import (
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
}

var hubJoinProbe = func(remote *loop.RemoteConfig, agentID, keyPath string) (hubwire.HelloOK, []string, error) {
	return hubclient.Probe(context.Background(), remote, agentID, version, keyPath)
}

var hubJoinAutoAuthorize = runHubJoinAutoAuthorize
var hubJoinReapMux = hubclient.ReapSSHMux
var hubJoinInstallShims = installHubJoinShims
var hubJoinHostname = os.Hostname
var hubJoinFingerprint = readHubJoinFingerprint
var hubJoinDiscoverFingerprint = discoverHubJoinFingerprint

func cmdHubJoin(args []string) error {
	fs := flag.NewFlagSet("hub join", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var opts hubJoinOptions
	fs.StringVar(&opts.Name, "name", "", "local wrapper name")
	fs.StringVar(&opts.AgentID, "as", "", "pool-wide agent id")
	fs.BoolVar(&opts.ResetHostKey, "reset-hostkey", false, "replace the pinned host key after operator confirmation")
	fs.BoolVar(&opts.RotateKey, "rotate-key", false, "rotate this joined identity's key")
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
	var oldHubID string
	completed := false
	err = withHubLock(remote.HubID, func() error {
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
		return runHubJoin(root, remote, opts)
	})
}

func runHubJoin(root string, remote *loop.RemoteConfig, opts hubJoinOptions) error {
	if opts.ResetHostKey {
		if err := os.Remove(filepath.Join(remote.HubDir, "known_hosts")); err != nil && !os.IsNotExist(err) {
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
	fingerprint, _ := hubJoinFingerprint(remote)
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
		return nil
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
	replaceArg := ""
	if replace {
		replaceArg = " --replace-key"
	}
	return fmt.Sprintf("Run this ON THE HUB, then retry here:\n  agentchute hub authorize --agent %s --pool %s --key %s%s", agentID, remote.PoolPath, strconv.Quote(pubkey), replaceArg)
}

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
	args := []string{}
	if remote.Port != 22 {
		args = append(args, "-p", strconv.Itoa(remote.Port))
	}
	args = append(args, remote.Destination(), strings.Join(quoted, " "))
	cmd := exec.Command("ssh", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
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
	if old != "" && old != url {
		fmt.Printf("pointer replaced: %s -> %s\n", old, url)
	} else {
		fmt.Printf("wrote pointer: %s (ignored via .git/info/exclude)\n", loop.PointerFileName)
	}
	return nil
}

func appendHubPointerExclude(root string) error {
	cmd := exec.Command("git", "-C", root, "rev-parse", "--git-path", "info/exclude")
	out, err := cmd.Output()
	if err != nil {
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
	if !pathContains(dir, os.Getenv("PATH")) {
		fmt.Printf("warning: add %s to PATH, then start a new shell before using `ac serve`\n", dir)
	}
	return setupEnsureShimPath(setupOptions{ShimDir: dir})
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

func discoverHubJoinFingerprint(remote *loop.RemoteConfig) (string, error) {
	args := []string{"-T", "5", "-t", "ed25519"}
	if remote.Port != 22 {
		args = append(args, "-p", strconv.Itoa(remote.Port))
	}
	args = append(args, remote.Host)
	keys, err := exec.Command("ssh-keyscan", args...).Output()
	if err != nil || len(keys) == 0 {
		return "", &hubclient.Error{Code: "E_CONNECT", Msg: fmt.Sprintf("hub: cannot reach %s:%d (connect failed after 5s). Check network/VPN/tailnet, then retry; `agentchute doctor` runs this same probe. (If this machine should no longer be joined to this hub, delete .agentchute-control-repo.)", remote.Host, remote.Port), Retriable: true, Cause: err}
	}
	cmd := exec.Command("ssh-keygen", "-lf", "-")
	cmd.Stdin = strings.NewReader(string(keys))
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		return "", fmt.Errorf("could not parse host-key fingerprint")
	}
	return fields[1], nil
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
