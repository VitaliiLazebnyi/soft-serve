package repo

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/ui/common"
	"github.com/charmbracelet/x/ansi"
)

type readmeTestRepo struct{ proto.Repository }

func (readmeTestRepo) Name() string { return "empty-repo" }

// An empty repository has no ref, so no readme is ever loaded. The Readme
// tab must show the quick start text instead of spinning forever.
func TestReadmeEmptyRepoStopsLoading(t *testing.T) {
	ctx := config.WithContext(context.Background(), config.DefaultConfig())
	r := NewReadme(common.NewCommon(ctx, 80, 40))

	// A readme shown before, for example before the repository was emptied,
	// must not leave its path behind.
	r.Update(RepoMsg(readmeTestRepo{}))
	r.Update(ReadmeMsg{Content: "# Old", Path: "docs/README.md"})
	r.Init()
	r.Update(EmptyRepoMsg{})

	view := ansi.Strip(r.View())
	if strings.Contains(view, "loading") {
		t.Fatalf("readme still shows the spinner:\n%s", view)
	}
	if !strings.Contains(view, "Quick Start") {
		t.Errorf("readme does not show the empty repository text:\n%s", view)
	}
	if got := r.StatusBarValue(); got != " " {
		t.Errorf("status bar value = %q, want no readme directory", got)
	}
}
