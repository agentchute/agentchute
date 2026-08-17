package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/agentchute/agentchute/internal/hubclient"
	"github.com/agentchute/agentchute/internal/loop"
	"github.com/agentchute/agentchute/internal/op"
)

// repoListFlag accumulates --working-repo flag occurrences.
type repoListFlag []string

func (r *repoListFlag) String() string { return strings.Join(*r, ",") }
func (r *repoListFlag) Set(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	*r = append(*r, v)
	return nil
}

// registerOpts is the input bundle for performRegister. Callers (cmdRegister,
// cmdBoot) parse flags then hand the values here; the helper does host
// detection, the existing-registration merge, and the write.
//
// Pull-only (simple-again Gate 6c): a registration publishes NO wake state, so
// there is no wake-method/wake-target input and no tmux/herdr autodetect. The
// `*Provided` booleans distinguish "flag explicitly cleared to empty" from
// "flag never supplied" — the merge logic for re-registers depends on it.
type registerOpts struct {
	AgentID, Vendor string
	Host            string
	Bio             string
	WorkingRepos    []string
	ServeToken      string
	Announce        bool
	Sweep           bool

	HostProvided   bool
	BioProvided    bool
	VendorProvided bool
}

// registerResult is performRegister's outcome.
//
// `Refreshed` follows the registration wire semantics (AGENTCHUTE.md §5): true
// whenever performRegister touched the registration file (whether fresh
// enrollment or an update to an existing registration). It is NOT a
// signal of "was there a prior registration"; that distinct semantic is
// `ExistingFound`, used only for UX (text-mode "Refreshed" vs "Registered"
// verb choice) and never serialized into a published wire format.
type registerResult struct {
	Reg           *loop.Registration
	InboxDir      string
	Refreshed     bool   // true on every successful registration write (AGENTCHUTE.md §5).
	ExistingFound bool   // true if a prior registration file existed before this call.
	ResolvedHost  string // post-merge host actually written
	Warnings      []string
	Announce      *op.AnnounceView
	Pending       int
}

// performRegister writes / refreshes a registration through the seam.
//
// It is a thin ADAPTER, deliberately keeping its shipped signature: thirteen
// existing call sites take registerOpts, one of them passing Bio/BioProvided —
// fields the reshaped request does not have — so no type alias could bridge it.
// It is a CALLER of internal/op, never a callee, so no cycle.
//
// The one piece of work it does itself is HOST RESOLUTION (D1a). With no --host
// flag the local semantics are not "preserve" but "substitute os.Hostname()",
// and a nil host resolved hub-side would record the HUB's hostname for every
// remote self-check — the wrong machine. So the resolution happens client-side,
// here, and the op treats what it receives as explicit.
func performRegister(cfg *loop.Config, opts registerOpts, now time.Time) (*registerResult, error) {
	host := opts.Host
	if !opts.HostProvided {
		if h, err := os.Hostname(); err == nil {
			host = h
		} else {
			fmt.Fprintf(os.Stderr, "warning: os.Hostname() failed (%v); registering with empty host\n", err)
		}
	}

	vendor := opts.Vendor
	var vendorPtr *string
	if cfg.Remote == nil || opts.VendorProvided {
		vendorPtr = &vendor
	}
	req := op.RegisterReq{
		Vendor:       vendorPtr,
		Host:         host,
		WorkingRepos: opts.WorkingRepos,
		ServeToken:   opts.ServeToken,
		Announce:     opts.Announce,
		Sweep:        opts.Sweep,
	}
	if opts.BioProvided {
		bio := opts.Bio
		req.Bio = &bio
	}

	var resp op.RegisterResp
	var err error
	if cfg.Remote != nil {
		session, openErr := openRemoteOneShot(cfg, opts.AgentID)
		if openErr != nil {
			return nil, openErr
		}
		resp, err = session.Register(req)
	} else {
		resp, err = op.Register(cfg, op.Context{ActorID: opts.AgentID}, req, now)
	}
	if err != nil {
		return nil, err
	}
	return &registerResult{
		Reg:           resp.Reg.Registration(),
		InboxDir:      resp.InboxDir,
		Refreshed:     resp.Refreshed,
		ExistingFound: resp.ExistingFound,
		ResolvedHost:  resp.ResolvedHost,
		Warnings:      resp.Warnings,
		Announce:      resp.Announce,
		Pending:       resp.Pending,
	}, nil
}

func openRemoteOneShot(cfg *loop.Config, agentID string) (*hubclient.OneShot, error) {
	session, err := hubclient.OpenOneShot(context.Background(), cfg.Remote, agentID, version)
	if err != nil {
		if hubclient.ErrorCode(err) == "E_CONNECT" {
			_ = hubclient.RecordConnectFailure(cfg.Remote, time.Now().UTC())
		}
		return nil, err
	}
	_ = hubclient.ClearConnectFailure(cfg.Remote)
	for _, warning := range session.Warnings() {
		fmt.Fprintf(os.Stderr, "warning: %s\n", warning)
	}
	return session, nil
}

func remoteHookCached(cfg *loop.Config) bool {
	if cfg == nil || cfg.Remote == nil {
		return false
	}
	cached, _, err := hubclient.ConnectFailureCached(cfg.Remote, time.Now().UTC())
	return err == nil && cached
}

func degradeRemoteHook(err error) bool {
	if hubclient.ErrorCode(err) != "E_CONNECT" {
		return false
	}
	fmt.Fprintln(os.Stderr, "hub unreachable; skipping (will retry next event)")
	return true
}

func cmdRegister(args []string) error {
	fs := flag.NewFlagSet("register", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var agentID, vendor, host, controlRepo, loopDir, bio string
	var announce bool
	var workingRepos repoListFlag
	fs.StringVar(&agentID, "as", "", "agent id to act as (or $AGENTCHUTE_AGENT_ID)")
	fs.StringVar(&vendor, "vendor", "", "vendor or origin (e.g., anthropic, openai, local, human)")
	fs.StringVar(&host, "host", "", "host this agent runs on (defaults to OS hostname)")
	fs.StringVar(&controlRepo, "control-repo", "", "control repo path (or AGENTCHUTE_CONTROL_REPO)")
	fs.StringVar(&loopDir, "loop-dir", "", "loop dir path (or AGENTCHUTE_LOOP_DIR)")
	fs.StringVar(&bio, "bio", "", "short self-description for the registration body (markdown allowed)")
	fs.BoolVar(&announce, "announce", false, "after registering, send a direct enrollment notification to every existing peer")
	fs.Var(&workingRepos, "working-repo", "additional repo this agent edits (repeatable)")

	if err := fs.Parse(args); err != nil {
		return registerUsage(err)
	}

	// Track which fields the caller explicitly named so re-running register
	// preserves existing registration values for fields the user did not pass.
	// Explicit "" still clears.
	opts := registerOpts{
		Host:         host,
		Bio:          bio,
		WorkingRepos: workingRepos,
		ServeToken:   os.Getenv("AGENTCHUTE_SERVE_TOKEN"),
	}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "host":
			opts.HostProvided = true
		case "bio":
			opts.BioProvided = true
		case "vendor":
			opts.VendorProvided = true
		}
	})

	if fs.NArg() != 0 {
		return registerUsage(fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " ")))
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	cfg, err := loop.Discover(loop.DiscoverOpts{
		ControlRepoFlag: controlRepo,
		LoopDirFlag:     loopDir,
		Cwd:             cwd,
		EnvControlRepo:  os.Getenv("AGENTCHUTE_CONTROL_REPO"),
		EnvLoopDir:      os.Getenv("AGENTCHUTE_LOOP_DIR"),
	})
	if err != nil {
		return err
	}

	agentID, err = resolveAgentID(agentID, cfg)
	if err != nil {
		return err
	}
	opts.AgentID = agentID
	if cfg.Remote != nil {
		opts.Vendor = strings.TrimSpace(vendor)
		opts.Announce = announce
	} else {
		opts.Vendor = resolveAgentVendor(vendor, agentID, cfg)
	}

	now := time.Now().UTC()
	result, err := performRegister(cfg, opts, now)
	if err != nil {
		return err
	}
	fmt.Printf("Registered %s\n", agentID)
	fmt.Printf("  vendor:        %s\n", result.Reg.Vendor)
	fmt.Printf("  host:          %s\n", result.ResolvedHost)
	fmt.Printf("  control_repo:  %s%s\n", cfg.ControlRepo, formatOriginSuffix(cfg.ControlRepoOrigin))
	fmt.Printf("  loop_dir:      %s%s\n", cfg.LoopDir, formatOriginSuffix(cfg.LoopDirOrigin))
	fmt.Printf("  registration:  %s\n", cfg.AgentRegistrationPath(agentID))
	fmt.Printf("  inbox:         %s\n", result.InboxDir)
	fmt.Println("  (pull-only: senders deliver to your inbox; you poll it yourself)")
	for _, w := range result.Warnings {
		fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	}

	if announce {
		if cfg.Remote != nil {
			if result.Announce != nil {
				for _, w := range result.Announce.Warnings {
					fmt.Fprintf(os.Stderr, "warning: %s\n", w)
				}
				if result.Announce.Total == 0 {
					fmt.Println("  announce:      no peers to announce to")
				} else {
					fmt.Printf("  announce:      sent to %d of %d peer(s)\n", result.Announce.Sent, result.Announce.Total)
				}
			}
		} else if ar, err := loop.AnnounceEnrollment(cfg, result.Reg); err != nil {
			fmt.Fprintf(os.Stderr, "warning: announce failed: %v\n", err)
		} else {
			for _, w := range ar.Warnings {
				fmt.Fprintf(os.Stderr, "warning: %s\n", w)
			}
			if ar.Total == 0 {
				fmt.Println("  announce:      no peers to announce to")
			} else {
				fmt.Printf("  announce:      sent to %d of %d peer(s)\n", ar.Sent, ar.Total)
			}
		}
	}
	return nil
}

func registerUsage(err error) error {
	return fmt.Errorf("%w\nusage: agentchute register --as <agent-id> --vendor <vendor> [--host <name>] [--bio <text>] [--announce] [--working-repo <path>]... [--control-repo <path>] [--loop-dir <path>]", err)
}
