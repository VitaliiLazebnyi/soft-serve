package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/webhook"
	"github.com/google/uuid"
	"github.com/matryer/is"
	"github.com/spf13/cobra"
)

// Webhook URLs are literal public addresses: validation accepts them without a
// DNS lookup. Tests that send to a webhook store a loopback URL for it
// directly (see whPointAtLoopback), which the SSRF-protected client refuses
// before any packet is sent.
const (
	whURL      = "https://93.184.216.34/hook"
	whURLOther = "https://93.184.216.35"
)

// whRepo creates the repository "hooks", owned by a fresh non-admin user, and
// returns that user's context.
func whRepo(t *testing.T, ctx context.Context, be *backend.Backend) context.Context {
	t.Helper()
	is := is.New(t)
	ownerCtx := withUser(t, ctx, be, "owner", false)
	_, err := be.CreateRepository(ownerCtx, "hooks", proto.UserFromContext(ownerCtx), proto.RepositoryOptions{})
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

// whSeq numbers the webhooks created by whCreate, so their URLs differ. It is
// atomic so the helper stays safe if tests are ever run in parallel.
var whSeq atomic.Int64

// whCreate creates a webhook on "hooks" with a URL unique to this call, since
// a repository cannot hold two webhooks with the same URL, and returns its ID.
func whCreate(t *testing.T, ctx context.Context, be *backend.Backend, extra ...string) int64 {
	t.Helper()
	is := is.New(t)
	url := fmt.Sprintf("%s?n=%d", whURL, whSeq.Add(1))
	args := append([]string{"webhook", "create", "hooks", url}, extra...)
	_, _, err := runRepoOutput(t, ctx, args...)
	is.NoErr(err)
	for _, h := range whHooks(t, ctx, be, "hooks") {
		if h.URL == url {
			return h.ID
		}
	}
	t.Fatalf("webhook %s not found", url)
	return 0
}

// whPointAtLoopback stores a loopback URL for the webhook without validation.
// Sends to it then fail in the SSRF-protected client before anything goes on
// the network, and are recorded as failed deliveries.
func whPointAtLoopback(t *testing.T, ctx context.Context, repo string, hookID int64) {
	t.Helper()
	is := is.New(t)
	be := backend.FromContext(ctx)
	r, err := be.Repository(ctx, repo)
	is.NoErr(err)
	h, err := be.Webhook(ctx, r, hookID)
	is.NoErr(err)
	is.NoErr(store.FromContext(ctx).UpdateWebhookByID(ctx, db.FromContext(ctx), r.ID(), hookID,
		"http://127.0.0.1:1/", h.Secret, int(h.ContentType), h.Active))
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

// Every invalid create input is refused for its own reason and leaves nothing
// behind. Each case names the error text it must fail with.
func TestWebhookCreateRejectsInvalidInput(t *testing.T) {
	cases := map[string]struct {
		args []string
		want string
	}{
		"unknown event":       {[]string{"webhook", "create", "hooks", whURL, "-e", "bogus"}, webhook.ErrInvalidEvent.Error()},
		"unknown content":     {[]string{"webhook", "create", "hooks", whURL, "-c", "xml"}, webhook.ErrInvalidContentType.Error()},
		"private target":      {[]string{"webhook", "create", "hooks", "http://127.0.0.1/x"}, webhook.ErrPrivateIP.Error()},
		"loopback hostname":   {[]string{"webhook", "create", "hooks", "http://localhost/x"}, webhook.ErrPrivateIP.Error()},
		"not a url":           {[]string{"webhook", "create", "hooks", "::not a url"}, "cannot parse URL"},
		"missing url":         {[]string{"webhook", "create", "hooks"}, "accepts 2 arg(s), received 1"},
		"missing repository":  {[]string{"webhook", "create"}, "accepts 2 arg(s), received 0"},
		"unsupported scheme":  {[]string{"webhook", "create", "hooks", "ftp://93.184.216.34/x"}, webhook.ErrInvalidScheme.Error()},
		"unknown repository":  {[]string{"webhook", "create", "nope", whURL}, proto.ErrRepoNotFound.Error()},
		"empty content type":  {[]string{"webhook", "create", "hooks", whURL, "-c", ""}, webhook.ErrInvalidContentType.Error()},
		"space-only content":  {[]string{"webhook", "create", "hooks", whURL, "-c", "   "}, webhook.ErrInvalidContentType.Error()},
		"bad event in middle": {[]string{"webhook", "create", "hooks", whURL, "-e", "push", "-e", "nope"}, webhook.ErrInvalidEvent.Error()},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			is := is.New(t)
			ctx, be := newAuthTestContext(t)
			ownerCtx := whRepo(t, ctx, be)
			before := whSnapshot(t, ownerCtx, be)

			_, _, err := runRepoOutput(t, ownerCtx, tc.args...)
			is.True(err != nil)
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
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
	_, err := be.CreateRepository(ownerCtx, "other", proto.UserFromContext(ownerCtx), proto.RepositoryOptions{})
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
		"-u", whURLOther+"/new", "-s", "new", "-c", "FORM", "-e", "repository", "-e", "collaborator", "-a", "false"))

	h := whHooks(t, ownerCtx, be, "hooks")[0]
	is.Equal(h.URL, whURLOther+"/new")
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
		"bad url scheme":   {"-u", "gopher://93.184.216.34/x"},
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
	whPointAtLoopback(t, ownerCtx, "hooks", id)
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

	is.NoErr(runRepo(t, ownerCtx, "webhook", "update", "hooks", id, "-u", "  "+whURLOther+"/padded  "))
	is.Equal(whHooks(t, ownerCtx, be, "hooks")[0].URL, whURLOther+"/padded")
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

// whPushRefs pushes one commit to the "hooks" repository as the branches main,
// feature and other, and the tag v1. main is the default branch.
func whPushRefs(t *testing.T, ctx context.Context) {
	t.Helper()
	is := is.New(t)
	bare := filepath.Join(config.FromContext(ctx).DataPath, "repos", "hooks.git")
	work := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...) //nolint:noctx
		cmd.Dir = work
		// Ignore the user's git config, which may sign or annotate tags.
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	git("commit", "-q", "--allow-empty", "-m", "first")
	git("tag", "v1")
	git("push", "-q", bare, "main", "main:feature", "main:other", "v1")
	is.NoErr(exec.Command("git", "--git-dir", bare, "symbolic-ref", "HEAD", "refs/heads/main").Run()) //nolint:noctx
}

// Branch delete, tag delete and default branch change send their events
// through the backend's background deliveries: the session ending does not
// cancel them, and once shutdown has closed the queue no new one starts.
func TestBranchAndTagEventsUseBackgroundDeliveries(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	ownerCtx := whRepo(t, ctx, be)
	whPushRefs(t, ownerCtx)
	id := whCreate(t, ownerCtx, be, "-e", "branch_tag_delete", "-e", "repository")
	whPointAtLoopback(t, ownerCtx, "hooks", id)

	// Each command runs in a session context that is cancelled right after it.
	run := func(args ...string) {
		t.Helper()
		sess, cancel := context.WithCancel(ownerCtx)
		is.NoErr(runRepo(t, sess, args...))
		cancel()
	}
	run("branch", "delete", "hooks", "feature")
	run("tag", "delete", "hooks", "v1")
	run("branch", "default", "hooks", "other")

	be.WaitForDeliveries(context.Background())
	r, err := be.Repository(ownerCtx, "hooks")
	is.NoErr(err)
	events := func() []string {
		ds, err := be.ListWebhookDeliveries(ownerCtx, r, id)
		is.NoErr(err)
		out := make([]string, 0, len(ds))
		for _, d := range ds {
			out = append(out, d.Event.String())
		}
		sort.Strings(out)
		return out
	}
	is.Equal(events(), []string{"branch_tag_delete", "branch_tag_delete", "repository"})

	// The queue is closed now, as during shutdown: the change still happens,
	// but its event is skipped.
	run("branch", "delete", "hooks", "main")
	is.Equal(events(), []string{"branch_tag_delete", "branch_tag_delete", "repository"})
}
