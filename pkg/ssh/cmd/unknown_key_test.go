package cmd

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"charm.land/ssh"
	"github.com/charmbracelet/keygen"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/spf13/cobra"
	gossh "golang.org/x/crypto/ssh"
)

// A session whose key belongs to no user gets an error that names the
// key's fingerprint and how to make ssh offer only the key passed with -i,
// instead of a bare "user not found".
func TestUnknownKeyErrorNamesFingerprint(t *testing.T) {
	ctx, _ := newAuthTestContext(t)
	kp, err := keygen.New("", keygen.WithKeyType(keygen.Ed25519))
	if err != nil {
		t.Fatal(err)
	}
	pk := kp.PublicKey()
	keyCtx := context.WithValue(ctx, ssh.ContextKeyPublicKey, pk)
	fp := gossh.FingerprintSHA256(pk)

	for _, tc := range []struct {
		name string
		cmd  func() *cobra.Command
		args []string
	}{
		{"info", InfoCommand, nil},
		{"pubkey list", PubkeyCommand, []string{"list"}},
		{"set-username", SetUsernameCommand, []string{"bob"}},
		{"token list", TokenCommand, []string{"list"}},
		{"jwt", JWTCommand, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.cmd()
			c.SetOut(&bytes.Buffer{})
			c.SetErr(&bytes.Buffer{})
			c.SetArgs(tc.args)
			err := c.ExecuteContext(keyCtx)
			if !errors.Is(err, proto.ErrUserNotFound) {
				t.Fatalf("got %v, want ErrUserNotFound", err)
			}
			for _, want := range []string{fp, "IdentitiesOnly=yes"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}

	// Without a public key, such as a keyboard-interactive session, there
	// is no key to name.
	if err := errUnknownKey(ctx); err.Error() != proto.ErrUserNotFound.Error() {
		t.Errorf("got %v, want ErrUserNotFound", err)
	}
}
