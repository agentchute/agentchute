package hubwire

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type HandshakeOptions struct {
	PinnedAgent string
	Pool        string
	Pool12      string
	HubBin      string
	HubMin      int
	HubMax      int
	Now         func() time.Time
}

func NegotiateHello(req Hello, opts HandshakeOptions) (HelloOK, error) {
	if req.T != "hello" || req.Proto != Protocol {
		return HelloOK{}, protocolError(CodeVersion, fmt.Sprintf("hub speaks %s v1; client requested %q", Protocol, req.Proto))
	}
	hubMin, hubMax := opts.HubMin, opts.HubMax
	if hubMin == 0 {
		hubMin = MinVersion
	}
	if hubMax == 0 {
		hubMax = Version
	}
	use := hubMax
	if req.V < use {
		use = req.V
	}
	if use < req.MinV || use < hubMin {
		return HelloOK{}, protocolError(CodeVersion, fmt.Sprintf("hub speaks %s v%d; client requires >=%d; upgrade the hub first", Protocol, hubMax, req.MinV))
	}
	if req.Agent != opts.PinnedAgent {
		return HelloOK{}, protocolError(CodeIdentity, fmt.Sprintf("key is authorized as %q; you are acting as %q", opts.PinnedAgent, req.Agent))
	}
	now := time.Now().UTC()
	if opts.Now != nil {
		now = opts.Now().UTC()
	}
	return HelloOK{
		ResponseBase: ResponseBase{T: "hello-ok", Re: req.ID},
		V:            use,
		Agent:        opts.PinnedAgent,
		Pool:         opts.Pool,
		Pool12:       opts.Pool12,
		Writable:     probeWritable(filepath.Join(opts.Pool, ".agentchute", "loop", "state")),
		HubBin:       opts.HubBin,
		HubTime:      now,
	}, nil
}

func probeWritable(dir string) bool {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false
	}
	f, err := os.CreateTemp(dir, ".hub-write-probe-*")
	if err != nil {
		return false
	}
	name := f.Name()
	closeErr := f.Close()
	removeErr := os.Remove(name)
	return closeErr == nil && removeErr == nil
}
