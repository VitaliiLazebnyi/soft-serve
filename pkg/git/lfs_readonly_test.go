package git

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/git-lfs-transfer/transfer"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// lfsTestRepo is a proto.Repository with just an ID and a name. Anything
// else panics, which is what a refused command must never reach.
type lfsTestRepo struct{ proto.Repository }

func (lfsTestRepo) ID() int64    { return 1 }
func (lfsTestRepo) Name() string { return "repo1" }

// lfsTestUser is a proto.User that only needs to be non-nil.
type lfsTestUser struct{ proto.User }

func pkt(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }

// A download session needs only read access, so it must not store objects
// or take locks, even though the transfer processor accepts put-object and
// lock in any session. The store and database are left out of the context on
// purpose: a refused command must not reach them.
func TestLFSTransferDownloadSessionRefusesWrites(t *testing.T) {
	data := "hello"
	sum := sha256.Sum256([]byte(data))
	oid := hex.EncodeToString(sum[:])

	var in strings.Builder
	in.WriteString(pkt("version 1\n") + "0000")
	in.WriteString(pkt("put-object "+oid+"\n") + pkt(fmt.Sprintf("size=%d\n", len(data))) + "0001")
	in.WriteString(pkt(data) + "0000")
	in.WriteString(pkt("lock\n") + pkt("path=foo.png\n") + pkt("refname=refs/heads/main\n") + "0000")
	in.WriteString(pkt("quit\n") + "0000")

	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	ctx := config.WithContext(context.Background(), cfg)
	ctx = proto.WithRepositoryContext(ctx, lfsTestRepo{})
	ctx = proto.WithUserContext(ctx, lfsTestUser{})

	var out bytes.Buffer
	if err := LFSTransfer(ctx, ServiceCommand{
		Stdin:  strings.NewReader(in.String()),
		Stdout: &out,
		Args:   []string{"repo1", lfs.OperationDownload},
	}); err != nil {
		t.Fatalf("LFSTransfer: %v", err)
	}

	var got []string
	for _, m := range regexp.MustCompile(`status (\d{3})`).FindAllStringSubmatch(out.String(), -1) {
		got = append(got, m[1])
	}
	// version, put-object, lock, quit. The object data is consumed, so it
	// is not read back as more commands.
	want := []string{"200", "403", "403", "200"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("statuses = %v, want %v\noutput: %q", got, want, out.String())
	}

	if _, err := os.Stat(filepath.Join(cfg.DataPath, "lfs", "1", "objects")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("object storage was written in a download session: %v", err)
	}
}

func TestLFSLockBackendDownloadSessionRefusesWrites(t *testing.T) {
	l := &lfsLockBackend{lfsTransfer: &lfsTransfer{op: lfs.OperationDownload}, user: lfsTestUser{}}
	if _, err := l.Create("foo.png", "refs/heads/main"); !errors.Is(err, transfer.ErrForbidden) {
		t.Errorf("Create: got %v, want ErrForbidden", err)
	}
	if err := l.Unlock(&LFSLock{}); !errors.Is(err, transfer.ErrForbidden) {
		t.Errorf("Unlock: got %v, want ErrForbidden", err)
	}
}
