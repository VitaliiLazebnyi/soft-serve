package git

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
)

type lfsAuthUser struct{ proto.User }

func (lfsAuthUser) ID() int64        { return 7 }
func (lfsAuthUser) Username() string { return "alice" }

type lfsAuthRepo struct{ proto.Repository }

func (lfsAuthRepo) Name() string { return "repo" }

// The token LFSAuthenticate signs is a bearer credential: it goes to the
// client and never into the logs, even at debug level.
func TestLFSAuthenticateDoesNotLogToken(t *testing.T) {
	var logs bytes.Buffer
	logger := log.New(&logs)
	logger.SetLevel(log.DebugLevel)

	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.SSH.KeyPath = filepath.Join(cfg.DataPath, "host_key")

	ctx := config.WithContext(context.Background(), cfg)
	ctx = log.WithContext(ctx, logger)
	ctx = proto.WithUserContext(ctx, lfsAuthUser{})
	ctx = proto.WithRepositoryContext(ctx, lfsAuthRepo{})

	var out bytes.Buffer
	if err := LFSAuthenticate(ctx, ServiceCommand{Stdout: &out, Args: []string{"repo", lfs.OperationDownload}}); err != nil {
		t.Fatalf("LFSAuthenticate: %v", err)
	}

	var res lfs.AuthenticateResponse
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	token, ok := strings.CutPrefix(res.Header["Authorization"], "Bearer ")
	if !ok || token == "" {
		t.Fatalf("no bearer token in response: %q", out.String())
	}

	got := logs.String()
	if !strings.Contains(got, "generated token") {
		t.Errorf("expected the debug line to be logged:\n%s", got)
	}
	if strings.Contains(got, token) {
		t.Errorf("log contains the signed token:\n%s", got)
	}
}
