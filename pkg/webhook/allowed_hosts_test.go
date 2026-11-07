package webhook

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/ssrf"
	"github.com/matryer/is"
)

// setAllowedHosts sets webhook.allowed_hosts on the test config and parses it
// the way the server does.
func (e *testEnv) setAllowedHosts(t *testing.T, entries ...string) {
	t.Helper()
	e.cfg.Webhook.AllowedHosts = entries
	if err := e.cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// TestSendWebhookAllowedHosts sends to a loopback receiver through the real
// SSRF-protected client: it is reached only while webhook.allowed_hosts
// covers it, and the list is read from the config at send time.
func TestSendWebhookAllowedHosts(t *testing.T) {
	is := is.New(t)
	e := newTestEnv(t)
	srv, rec := newReceiver(t, http.StatusOK, "")
	w := e.newWebhook(t, srv.URL+"/hook", "", ContentTypeJSON, EventBranchTagCreate)

	// Not listed: refused at send time, recorded with the reason.
	is.NoErr(SendWebhook(e.ctx, w, EventBranchTagCreate, samplePayload()))
	is.Equal(rec.calls, 0)
	ds := e.deliveries(t, w.ID)
	is.Equal(len(ds), 1)
	is.True(ds[0].RequestError.Valid)
	is.True(strings.Contains(ds[0].RequestError.String, ssrf.ErrPrivateIP.Error()))

	// Listed: delivered.
	e.setAllowedHosts(t, "127.0.0.0/8")
	is.NoErr(SendWebhook(e.ctx, w, EventBranchTagCreate, samplePayload()))
	is.Equal(rec.calls, 1)
	ds = e.deliveries(t, w.ID)
	is.Equal(len(ds), 2)
	for _, d := range ds {
		if d.ResponseStatus == http.StatusOK {
			is.True(!d.RequestError.Valid)
		}
	}

	// Removed from the list again: the stored webhook is refused, since
	// the current config applies to every delivery.
	e.setAllowedHosts(t, "10.0.0.0/8")
	err := sendWebhook(e.ctx, w, EventBranchTagCreate, samplePayload(), false)
	is.True(errors.Is(err, ssrf.ErrPrivateIP))
	is.Equal(rec.calls, 1)
}

// TestSendWebhookAllowedHostsKeepsMetadataBlocked checks that a broad entry
// does not open the cloud metadata address.
func TestSendWebhookAllowedHostsKeepsMetadataBlocked(t *testing.T) {
	is := is.New(t)
	e := newTestEnv(t)
	e.setAllowedHosts(t, "0.0.0.0/0")
	w := e.newWebhook(t, "http://169.254.169.254/latest/meta-data/", "", ContentTypeJSON, EventBranchTagCreate)
	err := sendWebhook(e.ctx, w, EventBranchTagCreate, samplePayload(), false)
	is.True(errors.Is(err, ssrf.ErrPrivateIP))
}

func TestValidateWebhookURLAllowing(t *testing.T) {
	is := is.New(t)
	allowed, err := ssrf.ParseAllowedHosts([]string{"127.0.0.1"})
	is.NoErr(err)
	is.NoErr(ValidateWebhookURLAllowing("http://127.0.0.1:8080/hook", allowed))
	is.True(errors.Is(ValidateWebhookURLAllowing("http://127.0.0.1:8080/hook", nil), ErrPrivateIP))
	is.True(errors.Is(ValidateWebhookURLAllowing("http://169.254.169.254/", allowed), ErrPrivateIP))
}
