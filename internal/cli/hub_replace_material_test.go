package cli

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHubAuthorizeReplaceKeyComparesMaterial(t *testing.T) {
	home, pool, _, key := setupHubAuthorizeTest(t)
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = null
	t.Cleanup(func() { os.Stdin = old; null.Close() })
	opts := hubAuthorizeOptions{Agent: "review-lane", Pool: pool, Key: key}
	if err := runHubAuthorize(opts, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".ssh", "authorized_keys")
	before := readAuthorizeTestFile(t, path)
	fields := strings.Fields(key)
	opts.ReplaceKey = true
	opts.Key = " \t" + fields[0] + "\t  " + fields[1] + " changed-comment  "
	if err := runHubAuthorize(opts, &bytes.Buffer{}); err != nil {
		t.Fatalf("same material with other whitespace/comment refused: %v", err)
	}
	if !bytes.Equal(before, readAuthorizeTestFile(t, path)) {
		t.Fatal("same material changed authorization")
	}
	opts.Key = fields[0] + " " + base64.StdEncoding.EncodeToString([]byte("different-key")) + " input-comment"
	err = runHubAuthorize(opts, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "interactive terminal") {
		t.Fatalf("different key with original comment: %v", err)
	}
	if !bytes.Equal(before, readAuthorizeTestFile(t, path)) {
		t.Fatal("refused replacement changed authorization")
	}
	t.Log("same material/different comment and whitespace accepted; different material/same comment refused on /dev/null")
}
