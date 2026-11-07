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

// A session refused by a server admin gate gets an error that names the
// key's fingerprint and how to make ssh offer only the key passed with -i,
// instead of a bare "unauthorized". The gate does not depend on repository
// state, so the extra detail reveals nothing about repositories.
func TestNotServerAdminErrorNamesFingerprint(t *testing.T) {
	ctx, be := newAuthTestContext(t)
	kp, err := keygen.New("", keygen.WithKeyType(keygen.Ed25519))
	if err != nil {
		t.Fatal(err)
	}
	pk := kp.PublicKey()
	fp := gossh.FingerprintSHA256(pk)

	// Both a key that belongs to no user and a non-admin user's key.
	unknownCtx := context.WithValue(ctx, ssh.ContextKeyPublicKey, pk)
	userCtx := context.WithValue(withUser(t, ctx, be, "bob", false), ssh.ContextKeyPublicKey, pk)

	for _, tc := range []struct {
		name string
		ctx  context.Context
		cmd  func() *cobra.Command
		args []string
	}{
		{"user list, unknown key", unknownCtx, UserCommand, []string{"list"}},
		{"user list, non-admin", userCtx, UserCommand, []string{"list"}},
		{"settings, non-admin", userCtx, SettingsCommand, []string{"anon-access"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.cmd()
			c.SetOut(&bytes.Buffer{})
			c.SetErr(&bytes.Buffer{})
			c.SetArgs(tc.args)
			err := c.ExecuteContext(tc.ctx)
			if !errors.Is(err, proto.ErrUnauthorized) {
				t.Fatalf("got %v, want ErrUnauthorized", err)
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
	if err := errNotServerAdmin(ctx); err.Error() != proto.ErrUnauthorized.Error() {
		t.Errorf("got %v, want ErrUnauthorized", err)
	}
}
