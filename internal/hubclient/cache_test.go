package hubclient

import (
	"os"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

func TestConnectFailureCacheWindowAndClear(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	remote, err := loop.ParseRemoteURL("ssh://hub.example/pool")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	if err := RecordConnectFailure(remote, now); err != nil {
		t.Fatal(err)
	}
	cached, age, err := ConnectFailureCached(remote, now.Add(29*time.Second))
	if err != nil || !cached || age != 29*time.Second {
		t.Fatalf("cached/age/err = %v/%s/%v", cached, age, err)
	}
	cached, _, err = ConnectFailureCached(remote, now.Add(30*time.Second))
	if err != nil || cached {
		t.Fatalf("expired cache = %v/%v", cached, err)
	}
	if err := ClearConnectFailure(remote); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(HubDownPath(remote)); !os.IsNotExist(err) {
		t.Fatalf("cache still exists: %v", err)
	}
}
