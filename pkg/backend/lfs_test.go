package backend

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

// fakeLFSClient serves every requested object from memory and records which
// objects were asked for.
type fakeLFSClient struct {
	objects   map[string][]byte
	requested []string
}

func (c *fakeLFSClient) Download(_ context.Context, pointers []lfs.Pointer, cb lfs.DownloadCallback) error {
	for _, p := range pointers {
		c.requested = append(c.requested, p.Oid)
		if err := cb(p, io.NopCloser(bytes.NewReader(c.objects[p.Oid])), nil); err != nil {
			return err
		}
	}
	return nil
}

func (c *fakeLFSClient) Upload(context.Context, []lfs.Pointer, lfs.UploadCallback) error {
	return nil
}

// TestStoreRepoMissingLFSObjects verifies that every LFS object is downloaded
// and recorded, including the final partial batch, and that a later sync only
// fetches objects that are missing on disk.
func TestStoreRepoMissingLFSObjects(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}

	for _, n := range []int{1, 21} {
		t.Run(fmt.Sprintf("%d objects", n), func(t *testing.T) {
			is := is.New(t)
			be, cfg := newTestBackend(t)
			ctx := config.WithContext(context.Background(), cfg)

			repo, err := be.CreateRepository(ctx, "lfs-repo", nil, proto.RepositoryOptions{})
			is.NoErr(err)

			client := &fakeLFSClient{objects: map[string][]byte{}}
			var pointers []lfs.Pointer
			work := t.TempDir()
			for i := 0; i < n; i++ {
				content := []byte(fmt.Sprintf("lfs object %d", i))
				p, err := lfs.GeneratePointer(bytes.NewReader(content))
				is.NoErr(err)
				client.objects[p.Oid] = content
				pointers = append(pointers, p)
				is.NoErr(os.WriteFile(filepath.Join(work, fmt.Sprintf("file%d.bin", i)), []byte(p.String()), 0o600))
			}

			git := func(args ...string) {
				t.Helper()
				cmd := exec.Command("git", args...)
				cmd.Dir = work
				cmd.Env = append(os.Environ(),
					"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
					"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
					"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
				)
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v\n%s", args, err, out)
				}
			}
			git("init", "-q", "-b", "main")
			git("add", ".")
			git("commit", "-q", "-m", "add lfs pointers")
			git("push", "-q", be.repoPath("lfs-repo"), "main")

			is.NoErr(StoreRepoMissingLFSObjects(ctx, repo, be.db, be.store, client))
			is.Equal(len(client.requested), n) // every LFS object is downloaded

			objPath := func(p lfs.Pointer) string {
				return filepath.Join(cfg.DataPath, "lfs", strconv.FormatInt(repo.ID(), 10), "objects", p.RelativePath())
			}
			for _, p := range pointers {
				_, err := be.store.GetLFSObjectByOid(ctx, be.db, repo.ID(), p.Oid)
				is.NoErr(err) // every LFS object is recorded
				got, err := os.ReadFile(objPath(p))
				is.NoErr(err)
				is.Equal(got, client.objects[p.Oid]) // every LFS object is stored
			}

			// A second sync finds everything on disk and recorded.
			client.requested = nil
			is.NoErr(StoreRepoMissingLFSObjects(ctx, repo, be.db, be.store, client))
			is.Equal(len(client.requested), 0) // nothing is downloaded again

			// An object that is recorded but gone from disk is fetched again.
			is.NoErr(os.Remove(objPath(pointers[0])))
			is.NoErr(StoreRepoMissingLFSObjects(ctx, repo, be.db, be.store, client))
			is.Equal(client.requested, []string{pointers[0].Oid})
			got, err := os.ReadFile(objPath(pointers[0]))
			is.NoErr(err)
			is.Equal(got, client.objects[pointers[0].Oid])
		})
	}
}
