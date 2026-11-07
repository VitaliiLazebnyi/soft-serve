package lfs

import (
	"encoding/json"
	"strings"
	"testing"
)

// expires_in on an action link is a number of seconds, both in what the
// server sends and in what the client reads from a remote server.
func TestLinkExpiresInSeconds(t *testing.T) {
	var l Link
	if err := json.Unmarshal([]byte(`{"href":"https://example.com/obj","expires_in":3600}`), &l); err != nil {
		t.Fatalf("decode link: %v", err)
	}
	if l.ExpiresIn == nil || *l.ExpiresIn != int64(3600) {
		t.Fatalf("expires_in = %v, want 3600 seconds", l.ExpiresIn)
	}

	b, err := json.Marshal(l)
	if err != nil {
		t.Fatalf("encode link: %v", err)
	}
	if !strings.Contains(string(b), `"expires_in":3600`) {
		t.Errorf("encoded link %s, want expires_in 3600", b)
	}
}
