package cli

// withHubLock and withHubLocks run fn while holding the hub locks EXCLUSIVELY.
// They are the migration/join side of the pair; a `serve` lane holds the same
// locks SHARED for its lifetime via acquireHubLocks.
func withHubLock(hubID string, fn func() error) error {
	return withHubLocks([]string{hubID}, fn)
}

func withHubLocks(hubIDs []string, fn func() error) error {
	release, err := acquireHubLocks(hubIDs, hubLockExclusive)
	if err != nil {
		return err
	}
	defer release()
	return fn()
}
