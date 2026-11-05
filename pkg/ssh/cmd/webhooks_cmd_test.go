package cmd

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/webhook"
	"github.com/google/uuid"
	"github.com/matryer/is"
	"github.com/spf13/cobra"
)

const whURL = "https://example.com/hook"

// whRepo creates the repository "hooks", owned by a fresh non-admin user, and
// returns that user's context.
func whRepo(t *testing.T, ctx context.Context, be *backend.Backend) context.Context {
	t.Helper()
	is := is.New(t)
	ownerCtx := withUser(t, ctx, be, "owner", false)
	_, err := seedCreate(be, ownerCtx, "hooks", proto.UserFromContext(ownerCtx), proto.RepositoryOptions{})
	is.NoErr(err)
	return ownerCtx
}

// whHooks returns the webhooks of repository "hooks" as the backend sees them.
func whHooks(t *testing.T, ctx context.Context, be *backend.Backend, repo string) []webhook.Hook {
	t.Helper()
	is := is.New(t)
	r, err := be.Repository(ctx, repo)
	is.NoErr(err)
	hooks, err := be.ListWebhooks(ctx, r)
	is.NoErr(err)
	return hooks
}

func eventNames(evs []webhook.Event) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.String())
	}
	sort.Strings(out)
	return out
}

// whSnapshot captures every stored field of the hooks of "hooks", so tests can
// assert that a failed command changed nothing.
func whSnapshot(t *testing.T, ctx context.Context, be *backend.Backend) string {
	t.Helper()
	var b strings.Builder
	for _, h := range whHooks(t, ctx, be, "hooks") {
		b.WriteString(strconv.FormatInt(h.ID, 10) + "|" + h.URL + "|" + h.Secret + "|" +
			strconv.FormatBool(h.Active) + "|" + h.ContentType.String() + "|" +
			strings.Join(eventNames(h.Events), ",") + "\n")
	}
	return b.String()
}

// whSeq numbers the webhooks created by whCreate, so their URLs differ.
var whSeq int

// whCreate creates a webhook on "hooks" with a URL unique to this call, since
// a repository cannot hold two webhooks with the same URL.
func whCreate(t *testing.T, ctx context.Context, be *backend.Backend, extra ...string) int64 {
	t.Helper()
	is := is.New(t)
	whSeq++
	url := fmt.Sprintf("%s?n=%d", whURL, whSeq)
	args := append([]string{"webhook", "create", "hooks", url}, extra...)
	_, _, err := runRepoOutput(t, ctx, args...)
	is.NoErr(err)
	hooks := whHooks(t, ctx, be, "hooks")
	return hooks[len(hooks)-1].ID
}

func whInsertDelivery(t *testing.T, ctx context.Context, hookID int64, id uuid.UUID, status int) {
	t.Helper()
	is := is.New(t)
	is.NoErr(store.FromContext(ctx).CreateWebhookDelivery(
		ctx, db.FromContext(ctx), id, hookID, int(webhook.EventPush),
		whURL, "POST", nil,
		"Content-Type: application/json\nX-SoftServe-Event: push\n", `{"ref":"main"}`,
		status, "X-Reply: yes\n", "reply body",
	))
}

func TestWebhookCreatePersistsEveryFlag(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)

	stdout, stderr, err := runRepoOutput(t, ownerCtx, "webhook", "create", "hooks", whURL,
		"-e", "push", "-e", "repository", "-s", "topsecret", "-c", "form", "-a=false")
	is.NoErr(err)
	is.Equal(stdout, "")
	is.Equal(stderr, "")

	hooks := whHooks(t, ownerCtx, be, "hooks")
	is.Equal(len(hooks), 1)
	h := hooks[0]
	is.Equal(h.URL, whURL)
	is.Equal(h.Secret, "topsecret")
	is.Equal(h.ContentType, webhook.ContentTypeForm)
	is.Equal(h.Active, false)
	is.Equal(eventNames(h.Events), []string{"push", "repository"})
}

func TestWebhookCreateDefaults(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)

	is.NoErr(runRepo(t, ownerCtx, "webhook", "create", "hooks", whURL))

	h := whHooks(t, ownerCtx, be, "hooks")[0]
	is.Equal(h.ContentType, webhook.ContentTypeJSON)
	is.Equal(h.Active, true)
	is.Equal(h.Secret, "")
	is.Equal(len(h.Events), 0)
}

func TestWebhookCreateContentTypeIsCaseInsensitiveAndTrimmed(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)

	whCreate(t, ownerCtx, be, "-c", " JSON ")
	whCreate(t, ownerCtx, be, "-c", "Form")

	hooks := whHooks(t, ownerCtx, be, "hooks")
	is.Equal(len(hooks), 2)
	is.Equal(hooks[0].ContentType, webhook.ContentTypeJSON)
	is.Equal(hooks[1].ContentType, webhook.ContentTypeForm)
}

func TestWebhookCreateTrimsURL(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)

	is.NoErr(runRepo(t, ownerCtx, "webhook", "create", "hooks", "  "+whURL+"  "))
	is.Equal(whHooks(t, ownerCtx, be, "hooks")[0].URL, whURL)
}

// Every invalid create input is refused and leaves nothing behind.
func TestWebhookCreateRejectsInvalidInput(t *testing.T) {
	cases := map[string][]string{
		"unknown event":       {"webhook", "create", "hooks", whURL, "-e", "bogus"},
		"unknown content":     {"webhook", "create", "hooks", whURL, "-c", "xml"},
		"private target":      {"webhook", "create", "hooks", "http://127.0.0.1/x"},
		"loopback hostname":   {"webhook", "create", "hooks", "http://localhost/x"},
		"not a url":           {"webhook", "create", "hooks", "::not a url"},
		"missing url":         {"webhook", "create", "hooks"},
		"missing repository":  {"webhook", "create"},
		"unsupported scheme":  {"webhook", "create", "hooks", "ftp://example.com/x"},
		"unknown repository":  {"webhook", "create", "nope", whURL},
		"empty content type":  {"webhook", "create", "hooks", whURL, "-c", ""},
		"space-only content":  {"webhook", "create", "hooks", whURL, "-c", "   "},
		"bad event in middle": {"webhook", "create", "hooks", whURL, "-e", "push", "-e", "nope"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			is := is.New(t)
			ctx, be := newAuthTestContext(t)
			ownerCtx := whRepo(t, ctx, be)
			before := whSnapshot(t, ownerCtx, be)

			_, _, err := runRepoOutput(t, ownerCtx, args...)
			is.True(err != nil)
			is.Equal(whSnapshot(t, ownerCtx, be), before)
		})
	}
}

func TestWebhookCreateReportsUnknownEventAndContentType(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)

	err := runRepo(t, ownerCtx, "webhook", "create", "hooks", whURL, "-e", "bogus")
	is.True(errors.Is(err, webhook.ErrInvalidEvent))

	err = runRepo(t, ownerCtx, "webhook", "create", "hooks", whURL, "-c", "xml")
	is.True(errors.Is(err, webhook.ErrInvalidContentType))
}

func TestWebhookListOutputsRowsWithoutSecrets(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)

	whCreate(t, ownerCtx, be, "-s", "hidden-secret", "-e", "push", "-a=false")
	whCreate(t, ownerCtx, be, "-e", "repository")

	stdout, _, err := runRepoOutput(t, ownerCtx, "webhook", "list", "hooks")
	is.NoErr(err)
	is.True(strings.Contains(stdout, "ID"))
	is.True(strings.Contains(stdout, "Events"))
	is.True(strings.Contains(stdout, "push"))
	is.True(strings.Contains(stdout, "repository"))
	is.True(strings.Contains(stdout, "false"))
	is.True(strings.Contains(stdout, "true"))
	is.True(strings.Contains(stdout, whURL))
	is.True(!strings.Contains(stdout, "hidden-secret"))
}

func TestWebhookListEmptyRepository(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)

	stdout, _, err := runRepoOutput(t, ownerCtx, "webhook", "list", "hooks")
	is.NoErr(err)
	is.True(strings.Contains(stdout, "ID"))
	is.True(!strings.Contains(stdout, whURL))
}

func TestWebhookDeleteRemovesOnlyThatWebhook(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)

	first := whCreate(t, ownerCtx, be)
	second := whCreate(t, ownerCtx, be)

	is.NoErr(runRepo(t, ownerCtx, "webhook", "delete", "hooks", strconv.FormatInt(first, 10)))
	hooks := whHooks(t, ownerCtx, be, "hooks")
	is.Equal(len(hooks), 1)
	is.Equal(hooks[0].ID, second)
}

func TestWebhookDeleteRejectsBadIDs(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	whCreate(t, ownerCtx, be)
	before := whSnapshot(t, ownerCtx, be)

	err := runRepo(t, ownerCtx, "webhook", "delete", "hooks", "abc")
	is.True(err != nil)
	is.True(strings.Contains(err.Error(), "invalid webhook ID"))

	is.True(runRepo(t, ownerCtx, "webhook", "delete", "hooks", "999999") != nil)
	is.Equal(whSnapshot(t, ownerCtx, be), before)
}

// A webhook ID from another repository must not be deletable through this
// repository.
func TestWebhookDeleteIsScopedToRepository(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	_, err := seedCreate(be, ownerCtx, "other", proto.UserFromContext(ownerCtx), proto.RepositoryOptions{})
	is.NoErr(err)
	otherHook := whCreate(t, ownerCtx, be)
	is.NoErr(runRepo(t, ownerCtx, "webhook", "create", "other", whURL))
	otherHooks := whHooks(t, ownerCtx, be, "other")
	is.Equal(len(otherHooks), 1)
	id := otherHooks[0].ID
	is.True(id != otherHook)

	is.True(runRepo(t, ownerCtx, "webhook", "delete", "hooks", strconv.FormatInt(id, 10)) != nil)
	is.Equal(len(whHooks(t, ownerCtx, be, "other")), 1)
}

func TestWebhookUpdateChangesOnlyRequestedFields(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	id := whCreate(t, ownerCtx, be, "-s", "old", "-e", "push", "-c", "json", "-a=true")
	ids := strconv.FormatInt(id, 10)

	originalURL := whHooks(t, ownerCtx, be, "hooks")[0].URL

	is.NoErr(runRepo(t, ownerCtx, "webhook", "update", "hooks", ids, "-a=false"))
	h := whHooks(t, ownerCtx, be, "hooks")[0]
	is.Equal(h.Active, false)
	is.Equal(h.URL, originalURL)
	is.Equal(h.Secret, "old")
	is.Equal(h.ContentType, webhook.ContentTypeJSON)
	is.Equal(eventNames(h.Events), []string{"push"})
}

func TestWebhookUpdateChangesEveryFlag(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	id := whCreate(t, ownerCtx, be, "-s", "old", "-e", "push", "-c", "json")
	ids := strconv.FormatInt(id, 10)

	is.NoErr(runRepo(t, ownerCtx, "webhook", "update", "hooks", ids,
		"-u", "https://example.org/new", "-s", "new", "-c", "FORM", "-e", "repository", "-e", "collaborator", "-a", "false"))

	h := whHooks(t, ownerCtx, be, "hooks")[0]
	is.Equal(h.URL, "https://example.org/new")
	is.Equal(h.Secret, "new")
	is.Equal(h.ContentType, webhook.ContentTypeForm)
	is.Equal(eventNames(h.Events), []string{"collaborator", "repository"})
	is.Equal(h.Active, false)
}

func TestWebhookUpdateRejectsInvalidInput(t *testing.T) {
	cases := map[string][]string{
		"bad active":       {"-a", "maybe"},
		"bad content type": {"-c", "xml"},
		"bad event":        {"-e", "bogus"},
		"private url":      {"-u", "http://10.0.0.1/x"},
		"loopback url":     {"-u", "http://127.0.0.1/x"},
		"bad url scheme":   {"-u", "gopher://example.com/x"},
	}
	for name, flags := range cases {
		t.Run(name, func(t *testing.T) {
			is := is.New(t)
			ctx, be := newAuthTestContext(t)
			ownerCtx := whRepo(t, ctx, be)
			id := whCreate(t, ownerCtx, be, "-e", "push")
			before := whSnapshot(t, ownerCtx, be)

			args := append([]string{"webhook", "update", "hooks", strconv.FormatInt(id, 10)}, flags...)
			_, _, err := runRepoOutput(t, ownerCtx, args...)
			is.True(err != nil)
			is.Equal(whSnapshot(t, ownerCtx, be), before)
		})
	}
}

func TestWebhookUpdateRejectsBadIDs(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	whCreate(t, ownerCtx, be)

	err := runRepo(t, ownerCtx, "webhook", "update", "hooks", "abc", "-a=false")
	is.True(err != nil)
	is.True(strings.Contains(err.Error(), "invalid webhook ID"))

	is.True(runRepo(t, ownerCtx, "webhook", "update", "hooks", "999999", "-a=false") != nil)
}

func TestWebhookDeliveriesListShowsStatusRows(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	id := whCreate(t, ownerCtx, be)

	okID := uuid.MustParse("00000000-0000-0000-0000-00000000000a")
	badID := uuid.MustParse("00000000-0000-0000-0000-00000000000b")
	noneID := uuid.MustParse("00000000-0000-0000-0000-00000000000c")
	whInsertDelivery(t, ctx, id, okID, 204)
	whInsertDelivery(t, ctx, id, badID, 500)
	whInsertDelivery(t, ctx, id, noneID, 0)

	stdout, _, err := runRepoOutput(t, ownerCtx, "webhook", "deliveries", "list", "hooks", strconv.FormatInt(id, 10))
	is.NoErr(err)
	for _, want := range []string{okID.String(), badID.String(), noneID.String(), "push"} {
		is.True(strings.Contains(stdout, want))
	}
	okRow := rowContaining(stdout, okID.String())
	badRow := rowContaining(stdout, badID.String())
	noneRow := rowContaining(stdout, noneID.String())
	is.True(strings.Contains(okRow, "✅"))
	is.True(strings.Contains(badRow, "❌"))
	is.True(strings.Contains(noneRow, "❌"))
}

// rowContaining returns the rendered table line that holds needle.
func rowContaining(out, needle string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
}

func TestWebhookDeliveriesListWithoutDeliveries(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	id := whCreate(t, ownerCtx, be)

	stdout, _, err := runRepoOutput(t, ownerCtx, "webhook", "deliveries", "list", "hooks", strconv.FormatInt(id, 10))
	is.NoErr(err)
	is.True(strings.Contains(stdout, "Status"))
}

func TestWebhookDeliveriesRejectBadIDs(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	id := strconv.FormatInt(whCreate(t, ownerCtx, be), 10)
	delID := "00000000-0000-0000-0000-000000000001"

	for _, args := range [][]string{
		{"webhook", "deliveries", "list", "hooks", "abc"},
		{"webhook", "deliveries", "get", "hooks", "abc", delID},
		{"webhook", "deliveries", "get", "hooks", id, "not-a-uuid"},
		{"webhook", "deliveries", "redeliver", "hooks", "abc", delID},
		{"webhook", "deliveries", "redeliver", "hooks", id, "not-a-uuid"},
	} {
		err := runRepo(t, ownerCtx, args...)
		is.True(err != nil)
	}
}

func TestWebhookDeliveriesGetPrintsEverySection(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	id := whCreate(t, ownerCtx, be)
	delID := uuid.MustParse("00000000-0000-0000-0000-0000000000d1")
	whInsertDelivery(t, ctx, id, delID, 200)

	stdout, _, err := runRepoOutput(t, ownerCtx, "webhook", "deliveries", "get", "hooks", strconv.FormatInt(id, 10), delID.String())
	is.NoErr(err)
	for _, want := range []string{
		"ID: " + delID.String(),
		"Event: push",
		"Request URL: " + whURL,
		"Request Method: POST",
		"Request Headers:",
		"  X-SoftServe-Event: push",
		"Request Body:",
		`  {"ref":"main"}`,
		"Response Status: 200",
		"Response Headers:",
		"  X-Reply: yes",
		"Response Body:",
		"  reply body",
	} {
		// Whole-line match: "Event: push" is a substring of the header line
		// "X-SoftServe-Event: push", so substring matching cannot tell them apart.
		is.True(hasLine(stdout, want))
	}
}

// hasLine reports whether out contains a line equal to want.
func hasLine(out, want string) bool {
	for _, line := range strings.Split(out, "\n") {
		if line == want {
			return true
		}
	}
	return false
}

// Each list row must describe its own webhook: the events are comma-joined and
// the active flag matches that webhook.
func TestWebhookListRowsDescribeEachWebhook(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	const inactiveURL = whURL + "?n=inactive"
	const activeURL = whURL + "?n=active"
	is.NoErr(runRepo(t, ownerCtx, "webhook", "create", "hooks", inactiveURL, "-e", "push", "-e", "repository", "-a=false"))
	is.NoErr(runRepo(t, ownerCtx, "webhook", "create", "hooks", activeURL, "-e", "collaborator"))

	stdout, _, err := runRepoOutput(t, ownerCtx, "webhook", "list", "hooks")
	is.NoErr(err)
	inactiveRow := rowContaining(stdout, inactiveURL)
	activeRow := rowContaining(stdout, activeURL)
	is.True(strings.Contains(inactiveRow, "push,repository"))
	is.True(strings.Contains(inactiveRow, "false"))
	is.True(!strings.Contains(inactiveRow, "true"))
	is.True(strings.Contains(activeRow, "collaborator"))
	is.True(strings.Contains(activeRow, "true"))
	is.True(!strings.Contains(activeRow, "false"))
}

// Updating only the URL or secret must keep an inactive webhook inactive.
func TestWebhookUpdateKeepsInactiveState(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	id := strconv.FormatInt(whCreate(t, ownerCtx, be, "-a=false"), 10)

	is.NoErr(runRepo(t, ownerCtx, "webhook", "update", "hooks", id, "-s", "rotated"))
	h := whHooks(t, ownerCtx, be, "hooks")[0]
	is.Equal(h.Active, false)
	is.Equal(h.Secret, "rotated")
}

// Delivery rows are successful only for 2xx statuses.
func TestWebhookDeliveriesStatusBoundaries(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	id := whCreate(t, ownerCtx, be)

	cases := map[int]string{199: "❌", 200: "✅", 299: "✅", 300: "❌"}
	ids := map[int]uuid.UUID{}
	n := 0
	for status := range cases {
		n++
		ids[status] = uuid.MustParse("00000000-0000-0000-0000-0000000001" + strconv.Itoa(10+n))
		whInsertDelivery(t, ctx, id, ids[status], status)
	}

	stdout, _, err := runRepoOutput(t, ownerCtx, "webhook", "deliveries", "list", "hooks", strconv.FormatInt(id, 10))
	is.NoErr(err)
	for status, mark := range cases {
		row := rowContaining(stdout, ids[status].String())
		is.True(strings.Contains(row, mark))
	}
}

func TestWebhookDeliveriesGetUnknownDelivery(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	id := strconv.FormatInt(whCreate(t, ownerCtx, be), 10)

	stdout, _, err := runRepoOutput(t, ownerCtx, "webhook", "deliveries", "get", "hooks", id, "00000000-0000-0000-0000-0000000000ff")
	is.True(err != nil)
	is.Equal(stdout, "")
}

// A delivery belonging to one webhook must not be readable or redeliverable
// through another webhook of the same repository.
func TestWebhookDeliveriesAreScopedToWebhook(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	a := whCreate(t, ownerCtx, be)
	b := whCreate(t, ownerCtx, be)
	delID := uuid.MustParse("00000000-0000-0000-0000-0000000000e1")
	whInsertDelivery(t, ctx, a, delID, 200)

	bs := strconv.FormatInt(b, 10)
	_, _, err := runRepoOutput(t, ownerCtx, "webhook", "deliveries", "get", "hooks", bs, delID.String())
	is.True(err != nil)
	is.True(runRepo(t, ownerCtx, "webhook", "deliveries", "redeliver", "hooks", bs, delID.String()) != nil)
}

func TestWebhookRedeliverRecordsNewDelivery(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	id := whCreate(t, ownerCtx, be)
	ids := strconv.FormatInt(id, 10)
	delID := uuid.MustParse("00000000-0000-0000-0000-0000000000f1")
	whInsertDelivery(t, ctx, id, delID, 200)

	is.NoErr(runRepo(t, ownerCtx, "webhook", "deliveries", "redeliver", "hooks", ids, delID.String()))

	stdout, _, err := runRepoOutput(t, ownerCtx, "webhook", "deliveries", "list", "hooks", ids)
	is.NoErr(err)
	is.Equal(strings.Count(stdout, "push"), 2)
}

func TestWebhookCommandsRequireAdminAccess(t *testing.T) {
	ctxBase, beBase := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctxBase, beBase)
	id := whCreate(t, ownerCtx, beBase)
	ids := strconv.FormatInt(id, 10)
	delID := "00000000-0000-0000-0000-0000000000a1"
	whInsertDelivery(t, ownerCtx, id, uuid.MustParse(delID), 200)

	reader := func(t *testing.T) context.Context {
		is := is.New(t)
		rc := withUser(t, ctxBase, beBase, "reader", false)
		is.NoErr(beBase.AddCollaborator(ownerCtx, "hooks", "reader", access.ReadOnlyAccess))
		return rc
	}
	stranger := func(t *testing.T) context.Context {
		return withUser(t, ctxBase, beBase, "stranger", false)
	}

	for name, who := range map[string]func(*testing.T) context.Context{"read-only collaborator": reader, "stranger": stranger} {
		t.Run(name, func(t *testing.T) {
			is := is.New(t)
			ctx := who(t)
			before := whSnapshot(t, ownerCtx, beBase)

			for _, args := range [][]string{
				{"webhook", "list", "hooks"},
				{"webhook", "create", "hooks", whURL},
				{"webhook", "update", "hooks", ids, "-a=false"},
				{"webhook", "delete", "hooks", ids},
				{"webhook", "deliveries", "list", "hooks", ids},
				{"webhook", "deliveries", "get", "hooks", ids, delID},
				{"webhook", "deliveries", "redeliver", "hooks", ids, delID},
			} {
				err := runRepo(t, ctx, args...)
				is.True(err != nil)
			}
			is.Equal(whSnapshot(t, ownerCtx, beBase), before)
		})
	}
}

func TestWebhookServerAdminCanManageAnyRepository(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	rootCtx := withUser(t, ctx, be, "root", true)

	is.NoErr(runRepo(t, rootCtx, "webhook", "create", "hooks", whURL, "-e", "push"))
	id := whHooks(t, ownerCtx, be, "hooks")[0].ID
	ids := strconv.FormatInt(id, 10)

	is.NoErr(runRepo(t, rootCtx, "webhook", "update", "hooks", ids, "-a=false"))
	is.Equal(whHooks(t, ownerCtx, be, "hooks")[0].Active, false)
	is.NoErr(runRepo(t, rootCtx, "webhook", "delete", "hooks", ids))
	is.Equal(len(whHooks(t, ownerCtx, be, "hooks")), 0)
}

func TestWebhookCommandsOnMissingRepository(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	rootCtx := withUser(t, ctx, be, "root", true)

	for _, c := range []context.Context{ownerCtx, rootCtx} {
		is.True(runRepo(t, c, "webhook", "list", "ghost") != nil)
		is.True(runRepo(t, c, "webhook", "create", "ghost", whURL) != nil)
		is.True(runRepo(t, c, "webhook", "delete", "ghost", "1") != nil)
	}
}

func TestWebhookSecretIsNeverPrinted(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	const secret = "never-print-me"

	stdout, stderr, err := runRepoOutput(t, ownerCtx, "webhook", "create", "hooks", whURL, "-s", secret)
	is.NoErr(err)
	is.True(!strings.Contains(stdout, secret) && !strings.Contains(stderr, secret))
	id := strconv.FormatInt(whHooks(t, ownerCtx, be, "hooks")[0].ID, 10)

	stdout, stderr, err = runRepoOutput(t, ownerCtx, "webhook", "update", "hooks", id, "-s", secret+"2")
	is.NoErr(err)
	is.True(!strings.Contains(stdout, secret) && !strings.Contains(stderr, secret))

	stdout, _, err = runRepoOutput(t, ownerCtx, "webhook", "list", "hooks")
	is.NoErr(err)
	is.True(!strings.Contains(stdout, secret))
	is.True(!strings.Contains(stdout, secret+"2"))
}

// create trims surrounding whitespace from the URL, so update should accept
// the same padded URL and store it trimmed.
func TestWebhookUpdateTrimsURLLikeCreate(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	id := strconv.FormatInt(whCreate(t, ownerCtx, be), 10)

	is.NoErr(runRepo(t, ownerCtx, "webhook", "update", "hooks", id, "-u", "  https://example.org/padded  "))
	is.Equal(whHooks(t, ownerCtx, be, "hooks")[0].URL, "https://example.org/padded")
}

// requireRepo normally resolves the repository before RunE runs. Each leaf
// command must still refuse to run when no repository was resolved.
func TestWebhookRunEFailsWithoutResolvedRepository(t *testing.T) {
	is := is.New(t)
	ctx, _ := newAuthTestContext(t)

	var leaves []*cobra.Command
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		if len(c.Commands()) == 0 && c.RunE != nil {
			leaves = append(leaves, c)
		}
		for _, s := range c.Commands() {
			walk(s)
		}
	}
	walk(webhookCommand())
	// list, create, delete, update, deliveries list, redeliver, get.
	is.Equal(len(leaves), 7)

	for _, c := range leaves {
		c.SetContext(ctx)
		err := c.RunE(c, []string{"hooks", "1", "00000000-0000-0000-0000-000000000001"})
		is.True(err != nil)
	}
}

func TestWebhookUpdateContentTypeJSON(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	id := strconv.FormatInt(whCreate(t, ownerCtx, be, "-c", "form"), 10)

	is.NoErr(runRepo(t, ownerCtx, "webhook", "update", "hooks", id, "-c", " Json "))
	is.Equal(whHooks(t, ownerCtx, be, "hooks")[0].ContentType, webhook.ContentTypeJSON)
}

// failingWebhookStore fails the webhook listing query and delegates everything
// else, so the repository check still passes and the list command runs.
type failingWebhookStore struct {
	store.Store
}

func (failingWebhookStore) GetWebhooksByRepoID(context.Context, db.Handler, int64) ([]models.Webhook, error) {
	return nil, errors.New("store unavailable")
}

func TestWebhookListReportsStoreFailure(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	failCtx := store.WithContext(ownerCtx, failingWebhookStore{Store: store.FromContext(ownerCtx)})

	stdout, _, err := runRepoOutput(t, failCtx, "webhook", "list", "hooks")
	is.True(err != nil)
	is.True(strings.Contains(err.Error(), "store unavailable"))
	is.Equal(stdout, "")
}
