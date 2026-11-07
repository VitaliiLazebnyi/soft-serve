package repo

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
)

type fakeRepo struct {
	private bool
}

func (r fakeRepo) ID() int64                      { return 1 }
func (r fakeRepo) Name() string                   { return "repo" }
func (r fakeRepo) ProjectName() string            { return "" }
func (r fakeRepo) Description() string            { return "" }
func (r fakeRepo) IsPrivate() bool                { return r.private }
func (r fakeRepo) IsMirror() bool                 { return false }
func (r fakeRepo) IsHidden() bool                 { return false }
func (r fakeRepo) UserID() int64                  { return 1 }
func (r fakeRepo) CreatedAt() time.Time           { return time.Time{} }
func (r fakeRepo) UpdatedAt() time.Time           { return time.Time{} }
func (r fakeRepo) Open() (*git.Repository, error) { return nil, nil }

var _ proto.Repository = fakeRepo{}

// The repository page header shows the repository's visibility next to its
// name, public as well as private.
func TestHeaderVisibilityMarker(t *testing.T) {
	for _, tc := range []struct {
		private      bool
		want, absent string
	}{
		{private: true, want: "🔒", absent: "🌐"},
		{private: false, want: "🌐", absent: "🔒"},
	} {
		r := &Repo{
			common:       common.NewCommon(context.Background(), 80, 24),
			selectedRepo: fakeRepo{private: tc.private},
		}
		header := r.headerView()
		if !strings.Contains(header, tc.want) {
			t.Errorf("private=%v: header %q lacks %q", tc.private, header, tc.want)
		}
		if strings.Contains(header, tc.absent) {
			t.Errorf("private=%v: header %q shows %q", tc.private, header, tc.absent)
		}
	}
}
