package backend

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/ssrf"
	"github.com/charmbracelet/soft-serve/pkg/webhook"
	"github.com/matryer/is"
)

// setWebhookAllowedHosts sets webhook.allowed_hosts and parses it the way the
// server does at startup.
func (e whEnv) setWebhookAllowedHosts(t *testing.T, entries ...string) {
	t.Helper()
	e.cfg.Webhook.AllowedHosts = entries
	if err := e.cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// With webhook.allowed_hosts covering a loopback receiver, a webhook to it can
// be created and is delivered to through the SSRF-protected client.
func TestWebhookAllowedHostsCreateAndDeliver(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-SoftServe-Event") == webhook.EventRepositoryVisibilityChange.String() {
			hits.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	url := srv.URL + "/hook"

	// Default: refused at create.
	err := e.be.CreateWebhook(e.ctx, e.repo, url, webhook.ContentTypeJSON, "", []webhook.Event{webhook.EventRepositoryVisibilityChange}, true)
	is.True(errors.Is(err, ssrf.ErrPrivateIP))

	e.setWebhookAllowedHosts(t, "127.0.0.0/8")
	id := e.createHook(t, url, webhook.EventRepositoryVisibilityChange)

	is.NoErr(e.be.SetPrivate(e.ctx, "repo", true))
	e.be.WaitForDeliveries(e.ctx)
	is.Equal(hits.Load(), int32(1))

	ds, err := e.be.ListWebhookDeliveries(e.ctx, e.repo, id)
	is.NoErr(err)
	is.Equal(len(ds), 1)
	is.Equal(ds[0].ResponseStatus, http.StatusOK)
	is.True(!ds[0].RequestError.Valid)
}

// Update validates the new URL against the list too, and the list does not
// open metadata addresses.
func TestWebhookAllowedHostsUpdate(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	id := e.createHook(t, whURL, webhook.EventPush)
	events := []webhook.Event{webhook.EventPush}

	err := e.be.UpdateWebhook(e.ctx, e.repo, id, "http://10.1.2.3/hook", webhook.ContentTypeJSON, "", events, true)
	is.True(errors.Is(err, ssrf.ErrPrivateIP))

	e.setWebhookAllowedHosts(t, "10.0.0.0/8", "0.0.0.0/0:8080")
	is.NoErr(e.be.UpdateWebhook(e.ctx, e.repo, id, "http://10.1.2.3/hook", webhook.ContentTypeJSON, "", events, true))

	err = e.be.UpdateWebhook(e.ctx, e.repo, id, "http://169.254.169.254:8080/latest/meta-data", webhook.ContentTypeJSON, "", events, true)
	is.True(errors.Is(err, ssrf.ErrPrivateIP))
	err = e.be.CreateWebhook(e.ctx, e.repo, "http://169.254.169.254:8080/", webhook.ContentTypeJSON, "", events, true)
	is.True(errors.Is(err, ssrf.ErrPrivateIP))

	h, err := e.be.Webhook(e.ctx, e.repo, id)
	is.NoErr(err)
	is.Equal(h.URL, "http://10.1.2.3/hook")
}
