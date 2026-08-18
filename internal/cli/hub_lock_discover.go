package cli

import (
	"sync"

	"github.com/agentchute/agentchute/internal/loop"
)

// discoverConfig is the ONE seam where a command learns it is talking to a hub,
// and therefore the one place to take the hub's shared lock.
//
// §13.9a makes it a MUST: any process opening a descriptor under a hub tree holds
// that hub's shared lock for as long as the descriptor lives. A remote one-shot
// does that in more places than it looks — writeSendSpool puts an unsent body in
// HubDir/spool, and every dial can have its ssh child create HubDir/known_hosts
// under StrictHostKeyChecking=accept-new. Deciding per command which ones touch
// the tree is twenty chances to be wrong once, so nobody decides: discovering a
// remote config takes the lock.
//
// THE SCOPE IS THE COMMAND, NOT THE DIAL. The defect that prompted this happens
// AFTER the transport fails — preserveRemoteSendBody runs on the error path, once
// hubclient has already returned. A lock taken and released around the dial would
// leave that reproduction open while looking like a fix. Main releases after the
// handler returns, which is the only place with the right lifetime.
//
// hub join and hub authorize deliberately do NOT come through here. They resolve
// their root with resolveInitRoot and take the same locks EXCLUSIVELY themselves;
// a shared acquisition from the same process would make their own exclusive
// acquire spin the full timeout and fail with a busy error naming their own lock.
// That exemption is structural today rather than a special case, and a row keeps
// it honest.
func discoverConfig(opts loop.DiscoverOpts) (*loop.Config, error) {
	cfg, err := loop.Discover(opts)
	if err != nil {
		return nil, err
	}
	if cfg.Remote == nil {
		return cfg, nil
	}
	release, lockErr := acquireHubLocks([]string{cfg.Remote.HubID}, hubLockShared)
	if lockErr != nil {
		return nil, lockErr
	}
	heldHubLocks.add(release)
	return cfg, nil
}

// heldHubLocks holds what discoverConfig acquired until the command ends. A
// command is one process, so this is process state by nature; the mutex is for
// the handlers that discover from more than one goroutine.
var heldHubLocks hubLockRegistry

type hubLockRegistry struct {
	mu       sync.Mutex
	releases []func()
}

func (r *hubLockRegistry) add(release func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.releases = append(r.releases, release)
}

// releaseHubLocksHeldByCommand is called by Main once the handler has returned,
// and by tests that drive handlers directly. Releasing twice is harmless.
func releaseHubLocksHeldByCommand() {
	heldHubLocks.mu.Lock()
	releases := heldHubLocks.releases
	heldHubLocks.releases = nil
	heldHubLocks.mu.Unlock()
	for i := len(releases) - 1; i >= 0; i-- {
		releases[i]()
	}
}
