package webhook

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/matryer/is"
)

func TestRedactURL(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"https://hooks.slack.com/services/T000/B000/XXXX", "https://hooks.slack.com/[redacted]"},
		{"https://example.com/hook?token=XXXX", "https://example.com/[redacted]"},
		{"https://example.com?token=XXXX", "https://example.com/[redacted]"},
		{"https://example.com/#XXXX", "https://example.com/[redacted]"},
		{"https://user:XXXX@example.com:8443/", "https://example.com:8443"},
		{"https://example.com", "https://example.com"},
		{"https://example.com/", "https://example.com"},
		{"http://[::1", "[redacted]"},
		{"not a url XXXX", "[redacted]"},
		{"", "[redacted]"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			is := is.New(t)
			is.Equal(redactURL(c.in), c.want)
		})
	}
}

func TestRedactURLErrorKeepsChain(t *testing.T) {
	is := is.New(t)
	in := &url.Error{Op: "Post", URL: "https://hooks.slack.com/services/T000/B000/XXXX", Err: context.DeadlineExceeded}

	err := redactURLError(in)
	is.True(!strings.Contains(err.Error(), "XXXX"))
	is.True(!strings.Contains(err.Error(), "services"))
	is.True(strings.Contains(err.Error(), "hooks.slack.com"))
	is.True(errors.Is(err, context.DeadlineExceeded))
	var uerr *url.Error
	is.True(errors.As(err, &uerr))
	is.True(uerr.Timeout())
	is.Equal(in.URL, "https://hooks.slack.com/services/T000/B000/XXXX") // the original is not changed

	other := errors.New("other")
	is.Equal(redactURLError(other), other)
	is.Equal(redactURLError(nil), nil)
}

// secretPath and secretQuery stand for the tokens receivers such as Slack put
// in a webhook URL.
const (
	secretPath  = "/services/T000/B000/PATHSECRET"
	secretQuery = "?token=QUERYSECRET"
)

func assertNoSecret(t *testing.T, s string) {
	t.Helper()
	for _, secret := range []string{"PATHSECRET", "QUERYSECRET", "services", "USERSECRET"} {
		if strings.Contains(s, secret) {
			t.Fatalf("%q contains %q", s, secret)
		}
	}
}

// A failed unrecorded send returns an error for the caller to log; it must
// not reveal the token in the webhook URL's path, query or user info.
func TestDeliverUnrecordedTransportErrorRedactsURL(t *testing.T) {
	is := is.New(t)
	useHTTPClient(t)
	e := newTestEnv(t)
	base := strings.Replace(closingServer(t), "http://", "http://user:USERSECRET@", 1)
	w := e.newWebhook(t, base+secretPath+secretQuery, "", ContentTypeJSON, EventPush)

	err := DeliverUnrecorded(e.ctx, []models.Webhook{w}, samplePayloadFor(e))
	is.True(err != nil)
	assertNoSecret(t, err.Error())
	is.True(strings.Contains(err.Error(), "[redacted]"))
}

// A send that runs out of time still reports a deadline error through the
// redacted error, and the recorded error text is redacted too.
func TestDeliverTimeoutRedactsURLAndKeepsDeadline(t *testing.T) {
	is := is.New(t)
	useHTTPClient(t)
	useSendTimeout(t, 200*time.Millisecond)
	e := newTestEnv(t)
	w := e.newWebhook(t, hangingServer(t)+secretPath+secretQuery, "", ContentTypeJSON, EventPush)

	err := DeliverUnrecorded(e.ctx, []models.Webhook{w}, samplePayloadFor(e))
	is.True(err != nil)
	assertNoSecret(t, err.Error())
	is.True(errors.Is(err, context.DeadlineExceeded))
	var nerr net.Error
	is.True(errors.As(err, &nerr))
	is.True(nerr.Timeout())

	is.NoErr(Deliver(e.ctx, []models.Webhook{w}, samplePayloadFor(e)))
	ds := e.deliveries(t, w.ID)
	is.Equal(len(ds), 1)
	is.True(strings.Contains(ds[0].RequestError.String, "deadline exceeded"))
	assertNoSecret(t, ds[0].RequestError.String)
}

// A cancelled send keeps context.Canceled in the chain.
func TestDeliverUnrecordedCancelKeepsCanceled(t *testing.T) {
	is := is.New(t)
	useHTTPClient(t)
	e := newTestEnv(t)
	srv, _ := newReceiver(t, 200, "")
	w := e.newWebhook(t, srv.URL+secretPath+secretQuery, "", ContentTypeJSON, EventPush)
	ctx, cancel := context.WithCancel(e.ctx)
	cancel()

	err := DeliverUnrecorded(ctx, []models.Webhook{w}, samplePayloadFor(e))
	is.True(err != nil)
	assertNoSecret(t, err.Error())
	is.True(errors.Is(err, context.Canceled))
}
