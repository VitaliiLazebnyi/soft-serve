package cmd

import (
	"strings"
	"testing"

	"github.com/charmbracelet/keygen"
	"github.com/charmbracelet/soft-serve/pkg/sshutils"
)

func TestSplitUserCreateKey(t *testing.T) {
	newKey := func() string {
		kp, err := keygen.New("", keygen.WithKeyType(keygen.Ed25519))
		if err != nil {
			t.Fatal(err)
		}
		return kp.AuthorizedKey()
	}
	ak := newKey()
	typ, blob, _ := strings.Cut(ak, " ")
	_, otherBlob, _ := strings.Cut(newKey(), " ")

	cases := []struct {
		name    string
		key     string
		args    []string
		user    string
		wantErr string
	}{
		{"quoted", ak, []string{"bob"}, "bob", ""},
		{"key after username", typ, []string{"bob", blob}, "bob", ""},
		{"key before username", typ, []string{blob, "bob"}, "bob", ""},
		{"comment after username", typ, []string{"bob", blob, "my", "laptop"}, "bob", ""},
		{"comment before username", typ, []string{blob, "laptop", "bob"}, "bob", ""},
		{"complete key and extra arg", ak, []string{"bob", "extra"}, "", "accepts 1 arg(s), received 2"},
		{"not a key", typ, []string{"bob", "notakey"}, "", "invalid public key"},
		{"ambiguous", typ, []string{blob, otherBlob}, "", "cannot tell the username"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			user, pk, err := splitUserCreateKey(tc.key, tc.args)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("got err %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if user != tc.user {
				t.Errorf("username = %q, want %q", user, tc.user)
			}
			if got := sshutils.MarshalAuthorizedKey(pk); got != ak {
				t.Errorf("key = %q, want %q", got, ak)
			}
		})
	}
}
