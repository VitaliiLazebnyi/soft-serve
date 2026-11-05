package cmd

// Admin-only creation: only server admins may create repositories.

import (
	"errors"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

// With the flag off, registered users may create repositories as upstream
// does, and the admin-only restriction does not apply.
func TestUsersCreateRepositoriesWhenFlagOff(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContextWith(t, func(c *config.Config) { c.AdminOnlyCreate = false })
	userCtx := withUser(t, ctx, be, "worker", false)

	is.NoErr(runRepo(t, userCtx, "create", "mine", "-p"))
	_, err := be.Repository(ctx, "mine")
	is.NoErr(err)
}

// With the flag off, a registered owner may rename their repository to a new
// name, as upstream allows.
func TestUsersRenameToNewNameWhenFlagOff(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContextWith(t, func(c *config.Config) { c.AdminOnlyCreate = false })
	userCtx := withUser(t, ctx, be, "worker", false)

	is.NoErr(runRepo(t, userCtx, "create", "mine"))
	is.NoErr(runRepo(t, userCtx, "rename", "mine", "renamed"))
	_, err := be.Repository(ctx, "renamed")
	is.NoErr(err)
	_, err = be.Repository(ctx, "mine")
	is.True(err != nil)
}

func TestOnlyAdminsCreateRepositories(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContextWith(t, func(c *config.Config) { c.AdminOnlyCreate = true })
	adminCtx := withUser(t, ctx, be, "boss", true)
	userCtx := withUser(t, ctx, be, "worker", false)

	// Non-admin: create and import of a new name are refused, nothing is made.
	is.True(errors.Is(runRepo(t, userCtx, "create", "junk"), proto.ErrUnauthorized))
	is.True(errors.Is(runRepo(t, userCtx, "create", "junk2", "-p"), proto.ErrUnauthorized))
	is.True(errors.Is(runRepo(t, userCtx, "import", "junk3", "https://example.invalid/x.git"), proto.ErrUnauthorized))
	for _, n := range []string{"junk", "junk2", "junk3"} {
		_, err := be.Repository(ctx, n)
		is.True(err != nil)
	}

	// Admin: create works.
	is.NoErr(runRepo(t, adminCtx, "create", "real", "-p"))
	_, err := be.Repository(ctx, "real")
	is.NoErr(err)

	// A read-write collaborator still works on an existing repo, but cannot rename it to a new name.
	is.NoErr(runRepo(t, adminCtx, "collab", "add", "real", "worker", "read-write"))
	is.NoErr(runRepo(t, userCtx, "description", "real", "hello"))
	is.True(errors.Is(runRepo(t, userCtx, "rename", "real", "elsewhere"), proto.ErrUnauthorized))
}
