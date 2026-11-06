package cmd

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/sshutils"
	"github.com/matryer/is"
	gossh "golang.org/x/crypto/ssh"
)

// slashFreeKey returns a public key whose authorized-key text has no "/",
// so the text can also be a file name.
func slashFreeKey(t *testing.T) (gossh.PublicKey, string) {
	t.Helper()
	for {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		signer, err := gossh.NewSignerFromKey(priv)
		if err != nil {
			t.Fatal(err)
		}
		ak := strings.TrimSpace(sshutils.MarshalAuthorizedKey(signer.PublicKey()))
		if !strings.Contains(ak, "/") {
			return signer.PublicKey(), ak
		}
	}
}

// IsPublicKeyAdmin uses the parsed admin keys the backend caches instead
// of parsing initial_admin_keys again, trying each entry as a file first,
// on every check. A file that appears later in the working directory,
// named like a listed key, does not replace that key.
func TestIsPublicKeyAdminUsesCachedKeys(t *testing.T) {
	is := is.New(t)
	pub, ak := slashFreeKey(t)
	ctx, _ := newAuthTestContextWith(t, func(c *config.Config) {
		c.InitialAdminKeys = []string{ak}
	})

	t.Chdir(t.TempDir())
	is.True(IsPublicKeyAdmin(ctx, pub))

	other, otherAK := slashFreeKey(t)
	is.NoErr(os.WriteFile(ak, []byte(otherAK), 0o600))

	is.True(IsPublicKeyAdmin(ctx, pub))    // still the listed key
	is.True(!IsPublicKeyAdmin(ctx, other)) // the file's key is not an admin
}
