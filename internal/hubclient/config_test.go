package hubclient

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/agentchute/agentchute/internal/loop"
)

func plantHubConfig(t *testing.T, hubID string, cfg HubConfig) {
	t.Helper()
	if err := WriteHubConfig(hubID, &cfg); err != nil {
		t.Fatal(err)
	}
}

func TestHubConfigRoundTripPermissionsAndUnknownFieldDrop(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	want := HubConfig{
		URL:      "ssh://alex@hub.example/pool",
		JoinedAs: []string{"codex-tiny"},
		Names:    map[string]string{"codex": "codex-tiny"},
		Pool:     "/pool",
		Pool12:   "0123456789ab",
	}
	plantHubConfig(t, "abcdef012345", want)

	path, err := loop.HubConfigPath("abcdef012345")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("config mode = %o, want 600", got)
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("config dir mode = %o, want 700", got)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.TrimSpace(string(raw))[:len(strings.TrimSpace(string(raw)))-1] + ",\n  \"future\": true\n}\n")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadHubConfig("abcdef012345")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("round trip = %#v, want %#v", *got, want)
	}
	if err := WriteHubConfig("abcdef012345", got); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "future") {
		t.Fatalf("unknown field survived rewrite: %s", raw)
	}
}

func TestReadHubConfigNotFoundDistinctFromParseError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := ReadHubConfig("missing000000"); !errors.Is(err, ErrHubConfigNotFound) {
		t.Fatalf("missing error = %v, want ErrHubConfigNotFound", err)
	}

	path, err := loop.HubConfigPath("invalid000000")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = ReadHubConfig("invalid000000")
	if err == nil || errors.Is(err, ErrHubConfigNotFound) {
		t.Fatalf("parse error = %v, must differ from not found", err)
	}
}
