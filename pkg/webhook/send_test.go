package webhook

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/google/uuid"
	"github.com/matryer/is"
)

// recorder is a receiver that remembers the last request it got.
type recorder struct {
	mu      sync.Mutex
	method  string
	header  http.Header
	body    string
	calls   int
	status  int
	resBody string
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	b, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.method = req.Method
	r.header = req.Header.Clone()
	r.body = string(b)
	r.calls++
	r.mu.Unlock()
	if r.status != 0 {
		w.WriteHeader(r.status)
	}
	if r.resBody != "" {
		_, _ = io.WriteString(w, r.resBody)
	}
}

func newReceiver(t *testing.T, status int, resBody string) (*httptest.Server, *recorder) {
	t.Helper()
	rec := &recorder{status: status, resBody: resBody}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	return srv, rec
}

// samplePayload has url tags, so it can be sent as JSON or as a form.
func samplePayload() BranchTagEvent {
	return BranchTagEvent{
		Ref:     "refs/heads/main",
		Before:  "0000000000000000000000000000000000000000",
		After:   "1111111111111111111111111111111111111111",
		Created: true,
		Common: Common{
			EventType:  EventBranchTagCreate,
			Repository: Repository{ID: 1, Name: "repo"},
			Sender:     User{ID: 1, Username: "alice"},
		},
	}
}

func TestSendWebhookJSONDelivered(t *testing.T) {
	is := is.New(t)
	useHTTPClient(t)
	e := newTestEnv(t)
	srv, rec := newReceiver(t, http.StatusOK, `{"ok":true}`)
	w := e.newWebhook(t, srv.URL, "", ContentTypeJSON, EventBranchTagCreate)

	is.NoErr(SendWebhook(e.ctx, w, EventBranchTagCreate, samplePayload()))

	is.Equal(rec.method, http.MethodPost)
	is.Equal(rec.header.Get("Content-Type"), ContentTypeJSON.String())
	is.Equal(rec.header.Get("X-SoftServe-Event"), EventBranchTagCreate.String())
	is.True(strings.HasPrefix(rec.header.Get("User-Agent"), "SoftServe/"))
	is.True(rec.header.Get("X-SoftServe-Delivery") != "")
	is.True(rec.header.Get("X-SoftServe-Signature") == "")

	var got BranchTagEvent
	is.NoErr(json.Unmarshal([]byte(rec.body), &got))
	is.Equal(got.Ref, "refs/heads/main")
	is.Equal(got.Sender.Username, "alice")

	ds := e.deliveries(t, w.ID)
	is.Equal(len(ds), 1)
	is.Equal(ds[0].ResponseStatus, http.StatusOK)
	is.Equal(ds[0].ResponseBody, `{"ok":true}`)
	is.Equal(ds[0].RequestURL, srv.URL)
	is.Equal(ds[0].RequestMethod, http.MethodPost)
	is.Equal(ds[0].RequestError.String, "")
	is.Equal(ds[0].Event, int(EventBranchTagCreate))
	is.True(strings.Contains(ds[0].RequestHeaders, "X-Softserve-Event: branch_tag_create"))
	is.True(strings.Contains(ds[0].ResponseHeaders, "Content-Type"))
}

func TestSendWebhookFormDelivered(t *testing.T) {
	is := is.New(t)
	useHTTPClient(t)
	e := newTestEnv(t)
	srv, rec := newReceiver(t, http.StatusNoContent, "")
	w := e.newWebhook(t, srv.URL, "", ContentTypeForm, EventBranchTagCreate)

	is.NoErr(SendWebhook(e.ctx, w, EventBranchTagCreate, samplePayload()))

	is.Equal(rec.header.Get("Content-Type"), ContentTypeForm.String())
	form, err := url.ParseQuery(rec.body)
	is.NoErr(err)
	is.Equal(form.Get("ref"), "refs/heads/main")
	is.Equal(form.Get("created"), "true")
	is.Equal(form.Get("sender[username]"), "alice")

	ds := e.deliveries(t, w.ID)
	is.Equal(len(ds), 1)
	is.Equal(ds[0].ResponseStatus, http.StatusNoContent)
}

func TestSendWebhookInvalidContentTypeRejected(t *testing.T) {
	is := is.New(t)
	useHTTPClient(t)
	e := newTestEnv(t)
	srv, rec := newReceiver(t, http.StatusOK, "")
	w := e.newWebhook(t, srv.URL, "", ContentTypeJSON, EventBranchTagCreate)
	w.ContentType = 99

	err := SendWebhook(e.ctx, w, EventBranchTagCreate, samplePayload())
	is.True(errors.Is(err, ErrInvalidContentType))
	is.Equal(rec.calls, 0)
	is.Equal(len(e.deliveries(t, w.ID)), 0)
}

func TestSendWebhookJSONEncodeErrorReturned(t *testing.T) {
	is := is.New(t)
	useHTTPClient(t)
	e := newTestEnv(t)
	srv, rec := newReceiver(t, http.StatusOK, "")
	w := e.newWebhook(t, srv.URL, "", ContentTypeJSON, EventBranchTagCreate)

	// Channels cannot be encoded as JSON.
	err := SendWebhook(e.ctx, w, EventBranchTagCreate, make(chan int))
	is.True(err != nil)
	is.Equal(rec.calls, 0)
	is.Equal(len(e.deliveries(t, w.ID)), 0)
}

func TestSendWebhookFormEncodeErrorReturned(t *testing.T) {
	is := is.New(t)
	useHTTPClient(t)
	e := newTestEnv(t)
	srv, rec := newReceiver(t, http.StatusOK, "")
	w := e.newWebhook(t, srv.URL, "", ContentTypeForm, EventBranchTagCreate)

	// query.Values only accepts structs (or maps), not a bare int.
	err := SendWebhook(e.ctx, w, EventBranchTagCreate, 5)
	is.True(err != nil)
	is.Equal(rec.calls, 0)
	is.Equal(len(e.deliveries(t, w.ID)), 0)
}

func TestSendWebhookSignatureMatchesBody(t *testing.T) {
	is := is.New(t)
	useHTTPClient(t)
	e := newTestEnv(t)
	srv, rec := newReceiver(t, http.StatusOK, "")
	w := e.newWebhook(t, srv.URL, "s3cret", ContentTypeJSON, EventBranchTagCreate)

	is.NoErr(SendWebhook(e.ctx, w, EventBranchTagCreate, samplePayload()))

	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write([]byte(rec.body))
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	is.Equal(rec.header.Get("X-SoftServe-Signature"), want)
}

func TestSendWebhookSignatureDependsOnSecret(t *testing.T) {
	is := is.New(t)
	useHTTPClient(t)
	e := newTestEnv(t)
	srv, rec := newReceiver(t, http.StatusOK, "")
	a := e.newWebhook(t, srv.URL, "one", ContentTypeJSON, EventBranchTagCreate)
	is.NoErr(SendWebhook(e.ctx, a, EventBranchTagCreate, samplePayload()))
	sigOne := rec.header.Get("X-SoftServe-Signature")

	b := e.newWebhook(t, srv.URL+"/two", "two", ContentTypeJSON, EventBranchTagCreate)
	is.NoErr(SendWebhook(e.ctx, b, EventBranchTagCreate, samplePayload()))
	sigTwo := rec.header.Get("X-SoftServe-Signature")

	is.True(sigOne != "")
	is.True(sigOne != sigTwo)
}

// closingServer reads each request and closes the connection without
// answering, so every request fails the same way.
func closingServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close() //nolint:errcheck
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestSendWebhookNetworkErrorRecorded(t *testing.T) {
	is := is.New(t)
	useHTTPClient(t)
	e := newTestEnv(t)
	w := e.newWebhook(t, closingServer(t), "", ContentTypeJSON, EventBranchTagCreate)

	is.NoErr(SendWebhook(e.ctx, w, EventBranchTagCreate, samplePayload()))

	ds := e.deliveries(t, w.ID)
	is.Equal(len(ds), 1)
	is.True(ds[0].RequestError.String != "")
	is.Equal(ds[0].ResponseStatus, 0)
	is.Equal(ds[0].ResponseBody, "")
}

func TestSendWebhookNonSuccessStatusRecorded(t *testing.T) {
	is := is.New(t)
	useHTTPClient(t)
	e := newTestEnv(t)
	srv, _ := newReceiver(t, http.StatusInternalServerError, "boom")
	w := e.newWebhook(t, srv.URL, "", ContentTypeJSON, EventBranchTagCreate)

	is.NoErr(SendWebhook(e.ctx, w, EventBranchTagCreate, samplePayload()))

	ds := e.deliveries(t, w.ID)
	is.Equal(len(ds), 1)
	is.Equal(ds[0].RequestError.String, "") // the request itself succeeded
	is.Equal(ds[0].ResponseStatus, http.StatusInternalServerError)
	is.Equal(ds[0].ResponseBody, "boom")
}

func TestSendWebhookInvalidURLRecorded(t *testing.T) {
	is := is.New(t)
	useHTTPClient(t)
	e := newTestEnv(t)
	w := e.newWebhook(t, "http://[::1", "", ContentTypeJSON, EventBranchTagCreate)

	is.NoErr(SendWebhook(e.ctx, w, EventBranchTagCreate, samplePayload()))

	ds := e.deliveries(t, w.ID)
	is.Equal(len(ds), 1)
	is.True(ds[0].RequestError.String != "")
	is.Equal(ds[0].ResponseStatus, 0)
}

// truncatingServer answers every request with a Content-Length longer than the
// body it sends, then closes the connection. Reading the body fails.
func truncatingServer(t *testing.T) string {
	t.Helper()
	is := is.New(t)
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	is.NoErr(err)
	t.Cleanup(func() { ln.Close() }) //nolint:errcheck
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close() //nolint:errcheck
				r := bufio.NewReader(c)
				req, err := http.ReadRequest(r)
				if err != nil {
					return
				}
				_, _ = io.Copy(io.Discard, req.Body)
				_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\nshort")
			}(conn)
		}
	}()
	return fmt.Sprintf("http://%s/hook", ln.Addr().String())
}

// Regression for suspect (b): a body-read error used to return before the
// delivery was recorded. The delivery must be recorded and the read error kept.
func TestSendWebhookBodyReadErrorStillRecordsDelivery(t *testing.T) {
	is := is.New(t)
	useHTTPClient(t)
	e := newTestEnv(t)
	w := e.newWebhook(t, truncatingServer(t), "", ContentTypeJSON, EventBranchTagCreate)

	err := SendWebhook(e.ctx, w, EventBranchTagCreate, samplePayload())
	is.NoErr(err)

	ds := e.deliveries(t, w.ID)
	is.Equal(len(ds), 1)
	is.Equal(ds[0].ResponseStatus, http.StatusOK)
	is.True(strings.Contains(ds[0].RequestError.String, "unexpected EOF"))
}

func TestSendEventNoWebhooksIsNoop(t *testing.T) {
	is := is.New(t)
	useHTTPClient(t)
	e := newTestEnv(t)

	is.NoErr(SendEvent(e.ctx, samplePayloadFor(e)))
}

func TestSendEventDeliversToSubscribedWebhooksOnly(t *testing.T) {
	is := is.New(t)
	useHTTPClient(t)
	e := newTestEnv(t)
	srv, rec := newReceiver(t, http.StatusOK, "")

	push := e.newWebhook(t, srv.URL+"/push", "", ContentTypeJSON, EventPush)
	other := e.newWebhook(t, srv.URL+"/collab", "", ContentTypeJSON, EventCollaborator)

	is.NoErr(SendEvent(e.ctx, samplePayloadFor(e)))

	is.Equal(rec.calls, 1)
	is.Equal(len(e.deliveries(t, push.ID)), 1)
	is.Equal(len(e.deliveries(t, other.ID)), 0)
}

func TestSendEventDeliversToEveryWebhook(t *testing.T) {
	is := is.New(t)
	useHTTPClient(t)
	e := newTestEnv(t)
	srv, rec := newReceiver(t, http.StatusOK, "")

	var ws []models.Webhook
	for i := 0; i < 3; i++ {
		ws = append(ws, e.newWebhook(t, fmt.Sprintf("%s/%d", srv.URL, i), "", ContentTypeJSON, EventPush))
	}

	is.NoErr(SendEvent(e.ctx, samplePayloadFor(e)))

	is.Equal(rec.calls, 3)
	for _, w := range ws {
		is.Equal(len(e.deliveries(t, w.ID)), 1)
	}
}

// BUG (suspect a): SendEvent returns on the first SendWebhook error, so the
// webhooks after the failing one are never delivered. Every subscribed webhook
// must be attempted, and the failure must still be reported.
func TestSendEventFailingWebhookDoesNotBlockOthers(t *testing.T) {
	is := is.New(t)
	useHTTPClient(t)
	e := newTestEnv(t)
	srv, rec := newReceiver(t, http.StatusOK, "")

	bad := e.newWebhook(t, srv.URL+"/bad", "", ContentTypeJSON, EventPush)
	// Stored with an unknown content type: SendWebhook fails for it.
	is.NoErr(e.ds.UpdateWebhookByID(e.ctx, e.dbx, e.repo.ID, bad.ID, bad.URL, "", 99, true))
	good := e.newWebhook(t, srv.URL+"/good", "", ContentTypeJSON, EventPush)

	err := SendEvent(e.ctx, samplePayloadFor(e))
	is.True(err != nil)
	is.Equal(len(e.deliveries(t, good.ID)), 1)
	is.Equal(rec.calls, 1)
}

func TestSendEventWebhookLookupErrorReturned(t *testing.T) {
	is := is.New(t)
	useHTTPClient(t)
	e := newTestEnv(t)
	is.NoErr(e.dbx.Close())

	is.True(SendEvent(e.ctx, samplePayloadFor(e)) != nil)
}

func TestSendWebhookRecordErrorReturned(t *testing.T) {
	is := is.New(t)
	useHTTPClient(t)
	e := newTestEnv(t)
	srv, _ := newReceiver(t, http.StatusOK, "")
	w := e.newWebhook(t, srv.URL, "", ContentTypeJSON, EventPush)
	is.NoErr(e.dbx.Close())

	is.True(SendWebhook(e.ctx, w, EventPush, samplePayload()) != nil)
}

// samplePayloadFor is an event for the test repository, with the owner set.
func samplePayloadFor(e *testEnv) EventPayload {
	return PushEvent{
		Ref:    "refs/heads/main",
		Before: "0000000000000000000000000000000000000000",
		After:  "1111111111111111111111111111111111111111",
		Common: Common{
			EventType:  EventPush,
			Repository: Repository{ID: e.repo.ID, Name: e.repo.Name},
			Sender:     User{ID: e.owner.ID, Username: e.owner.Username},
		},
	}
}

// Every request and response header must be recorded, not only the last one.
func TestSendWebhookRecordsEveryHeader(t *testing.T) {
	is := is.New(t)
	useHTTPClient(t)
	e := newTestEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Receiver-A", "alpha")
		w.Header().Set("X-Receiver-B", "beta")
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)
	w := e.newWebhook(t, srv.URL, "secret", ContentTypeJSON, EventBranchTagCreate)

	is.NoErr(SendWebhook(e.ctx, w, EventBranchTagCreate, samplePayload()))

	ds := e.deliveries(t, w.ID)
	is.Equal(len(ds), 1)
	for _, want := range []string{
		"Content-Type: application/json",
		"User-Agent: SoftServe/",
		"X-Softserve-Event: branch_tag_create",
		"X-Softserve-Delivery: ",
		"X-Softserve-Signature: sha256=",
	} {
		is.True(strings.Contains(ds[0].RequestHeaders, want))
	}
	is.True(strings.Contains(ds[0].ResponseHeaders, "X-Receiver-A: alpha"))
	is.True(strings.Contains(ds[0].ResponseHeaders, "X-Receiver-B: beta"))
	is.Equal(ds[0].ResponseStatus, http.StatusAccepted)
}

// useSendTimeout shortens the per-webhook send timeout for the test. Like
// useHTTPClient it swaps a package variable, so tests using it must not call
// t.Parallel.
func useSendTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	orig := sendTimeout
	sendTimeout = d
	t.Cleanup(func() { sendTimeout = orig })
}

// hangingServer accepts requests and never answers them until the test ends.
func hangingServer(t *testing.T) string {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	return srv.URL
}

// Each webhook gets its own send timeout: a receiver that never answers uses
// up its own time only, and the webhooks after it are still sent and recorded.
func TestDeliverSlowWebhookDoesNotStarveOthers(t *testing.T) {
	is := is.New(t)
	useHTTPClient(t)
	useSendTimeout(t, 200*time.Millisecond)
	e := newTestEnv(t)
	srv, rec := newReceiver(t, http.StatusOK, "")
	slow := e.newWebhook(t, hangingServer(t), "", ContentTypeJSON, EventPush)
	fast := e.newWebhook(t, srv.URL, "", ContentTypeJSON, EventPush)

	is.NoErr(Deliver(e.ctx, []models.Webhook{slow, fast}, samplePayloadFor(e)))

	slowDs := e.deliveries(t, slow.ID)
	is.Equal(len(slowDs), 1)
	is.True(strings.Contains(slowDs[0].RequestError.String, "deadline exceeded"))
	fastDs := e.deliveries(t, fast.ID)
	is.Equal(len(fastDs), 1)
	is.Equal(fastDs[0].ResponseStatus, http.StatusOK)
	is.Equal(rec.calls, 1)
}

// A send whose context is already done fails, but the attempt is still
// recorded: the row is written with a context of its own.
func TestDeliverRecordsAttemptAfterContextDone(t *testing.T) {
	is := is.New(t)
	useHTTPClient(t)
	e := newTestEnv(t)
	srv, rec := newReceiver(t, http.StatusOK, "")
	w := e.newWebhook(t, srv.URL, "", ContentTypeJSON, EventPush)
	ctx, cancel := context.WithCancel(e.ctx)
	cancel()

	is.NoErr(Deliver(ctx, []models.Webhook{w}, samplePayloadFor(e)))

	ds := e.deliveries(t, w.ID)
	is.Equal(len(ds), 1)
	is.True(strings.Contains(ds[0].RequestError.String, "context canceled"))
	is.Equal(rec.calls, 0)
}

// Unrecorded deliveries have no row to show a failed status, so a non-2xx
// response is returned as an error for the caller to log.
func TestDeliverUnrecordedReportsNonSuccessStatus(t *testing.T) {
	is := is.New(t)
	useHTTPClient(t)
	e := newTestEnv(t)
	bad, _ := newReceiver(t, http.StatusInternalServerError, "boom")
	good, _ := newReceiver(t, http.StatusNoContent, "")
	badHook := e.newWebhook(t, bad.URL, "", ContentTypeJSON, EventPush)
	goodHook := e.newWebhook(t, good.URL, "", ContentTypeJSON, EventPush)

	err := DeliverUnrecorded(e.ctx, []models.Webhook{badHook}, samplePayloadFor(e))
	is.True(err != nil)
	is.True(strings.Contains(err.Error(), "status 500"))
	is.NoErr(DeliverUnrecorded(e.ctx, []models.Webhook{goodHook}, samplePayloadFor(e)))

	is.Equal(len(e.deliveries(t, badHook.ID)), 0)
	is.Equal(len(e.deliveries(t, goodHook.ID)), 0)
}

// RecordUndelivered stores the request it would have sent, with the reason as
// its error, and sends nothing.
func TestRecordUndeliveredStoresRequestWithoutSending(t *testing.T) {
	is := is.New(t)
	useHTTPClient(t)
	e := newTestEnv(t)
	srv, rec := newReceiver(t, http.StatusOK, "")
	w := e.newWebhook(t, srv.URL, "s3cret", ContentTypeJSON, EventPush)
	payload := samplePayloadFor(e)

	is.NoErr(RecordUndelivered(e.ctx, []models.Webhook{w}, payload, errors.New("queue full")))

	is.Equal(rec.calls, 0)
	ds := e.deliveries(t, w.ID)
	is.Equal(len(ds), 1)
	is.Equal(ds[0].RequestError.String, "queue full")
	is.Equal(ds[0].ResponseStatus, 0)
	is.Equal(ds[0].Event, int(EventPush))
	want, err := json.Marshal(payload)
	is.NoErr(err)
	is.Equal(strings.TrimSpace(ds[0].RequestBody), string(want))
	is.True(strings.Contains(ds[0].RequestHeaders, "X-Softserve-Signature: sha256="))
}

// useRetention sets how many deliveries each webhook keeps for the test. It
// swaps a package variable, so tests using it must not call t.Parallel.
func useRetention(t *testing.T, keep int) {
	t.Helper()
	orig := maxDeliveries
	maxDeliveries = keep
	t.Cleanup(func() { maxDeliveries = orig })
}

// Undelivered events cost no network time, so they are the quickest way to
// grow the deliveries table. Every record prunes, so a webhook never holds
// more than the limit, and pruning one webhook leaves the others' deliveries
// alone.
func TestRecordedDeliveriesAreBoundedPerWebhook(t *testing.T) {
	is := is.New(t)
	useRetention(t, 5)
	e := newTestEnv(t)
	a := e.newWebhook(t, "https://93.184.216.34/a", "", ContentTypeJSON, EventPush)
	b := e.newWebhook(t, "https://93.184.216.34/b", "", ContentTypeJSON, EventPush)
	payload := samplePayloadFor(e)

	var recorded []uuid.UUID
	for i := range 12 {
		is.NoErr(RecordUndelivered(e.ctx, []models.Webhook{a}, payload, errors.New("queue full")))
		ds := e.deliveries(t, a.ID)
		is.Equal(len(ds), min(i+1, 5))
		// The delivery just recorded is always kept.
		recorded = append(recorded, ds[len(ds)-1].ID)
	}
	for range 3 {
		is.NoErr(RecordUndelivered(e.ctx, []models.Webhook{b}, payload, errors.New("queue full")))
	}

	// The five kept are the five most recent, oldest first.
	var kept []uuid.UUID
	for _, d := range e.deliveries(t, a.ID) {
		kept = append(kept, d.ID)
	}
	is.Equal(kept, recorded[len(recorded)-5:])
	is.Equal(len(e.deliveries(t, b.ID)), 3)
}

// Production delivery IDs are time-based (version 1) UUIDs, whose text does
// not sort by time, so retention must not depend on them. Deliveries with
// IDs in descending order are still pruned oldest first.
func TestRecordedDeliveriesPruneIgnoresIDOrder(t *testing.T) {
	is := is.New(t)
	useRetention(t, 2)
	e := newTestEnv(t)
	w := e.newWebhook(t, "https://93.184.216.34/a", "", ContentTypeJSON, EventPush)
	ids := []uuid.UUID{
		uuid.MustParse("ffffffff-0000-1000-8000-000000000000"),
		uuid.MustParse("88888888-0000-1000-8000-000000000000"),
		uuid.MustParse("00000000-0000-1000-8000-000000000000"),
	}
	for _, id := range ids {
		req, err := newRequest(w, EventPush, samplePayloadFor(e))
		is.NoErr(err)
		req.id = id
		is.NoErr(recordDelivery(e.ctx, w, EventPush, req, nil, 200, "", ""))
	}
	ds := e.deliveries(t, w.ID)
	is.Equal(len(ds), 2)
	is.Equal(ds[0].ID, ids[1])
	is.Equal(ds[1].ID, ids[2])
}

// failingPruneStore is a store whose prune always fails.
type failingPruneStore struct{ store.Store }

func (failingPruneStore) PruneWebhookDeliveries(context.Context, db.Handler, int64, int) error {
	return errors.New("prune failed")
}

// A failed prune rolls back the transaction it shares with the insert, but
// must not lose the delivery: it is recorded again without pruning, and the
// failure is logged through the context's logger.
func TestRecordDeliveryKeepsDeliveryWhenPruneFails(t *testing.T) {
	is := is.New(t)
	e := newTestEnv(t)
	w := e.newWebhook(t, "https://93.184.216.34/a", "", ContentTypeJSON, EventPush)
	var logs bytes.Buffer
	ctx := store.WithContext(e.ctx, failingPruneStore{e.ds})
	ctx = log.WithContext(ctx, log.New(&logs))

	req, err := newRequest(w, EventPush, samplePayloadFor(e))
	is.NoErr(err)
	is.NoErr(recordDelivery(ctx, w, EventPush, req, nil, 200, "", ""))

	ds := e.deliveries(t, w.ID)
	is.Equal(len(ds), 1)
	is.Equal(ds[0].ID, req.id)
	is.True(strings.Contains(logs.String(), "pruning webhook deliveries"))
	is.True(strings.Contains(logs.String(), "prune failed"))
}

// Deliver re-reads each webhook before sending, so an event that waited while
// the webhook was changed goes to its current URL, signed with its current
// secret, and not to the old receiver.
func TestDeliverUsesCurrentWebhook(t *testing.T) {
	is := is.New(t)
	useHTTPClient(t)
	e := newTestEnv(t)
	oldSrv, oldRec := newReceiver(t, http.StatusOK, "")
	newSrv, newRec := newReceiver(t, http.StatusOK, "")
	w := e.newWebhook(t, oldSrv.URL, "old-secret", ContentTypeJSON, EventPush)
	is.NoErr(e.ds.UpdateWebhookByID(e.ctx, e.dbx, e.repo.ID, w.ID, newSrv.URL, "new-secret", int(ContentTypeJSON), true))

	is.NoErr(Deliver(e.ctx, []models.Webhook{w}, samplePayloadFor(e)))

	is.Equal(oldRec.calls, 0)
	is.Equal(newRec.calls, 1)
	mac := hmac.New(sha256.New, []byte("new-secret"))
	mac.Write([]byte(newRec.body))
	is.Equal(newRec.header.Get("X-SoftServe-Signature"), "sha256="+hex.EncodeToString(mac.Sum(nil)))
	ds := e.deliveries(t, w.ID)
	is.Equal(len(ds), 1)
	is.Equal(ds[0].RequestURL, newSrv.URL)
}

// A webhook deleted, or no longer subscribed to the event, after the event was
// read is not sent to.
func TestDeliverSkipsRemovedOrUnsubscribedWebhook(t *testing.T) {
	is := is.New(t)
	useHTTPClient(t)
	e := newTestEnv(t)
	srv, rec := newReceiver(t, http.StatusOK, "")
	deleted := e.newWebhook(t, srv.URL, "", ContentTypeJSON, EventPush)
	unsubscribed := e.newWebhook(t, srv.URL+"/?other", "", ContentTypeJSON, EventPush)
	is.NoErr(e.ds.DeleteWebhookForRepoByID(e.ctx, e.dbx, e.repo.ID, deleted.ID))
	is.NoErr(e.ds.DeleteWebhookEventsByWebhookID(e.ctx, e.dbx, unsubscribed.ID))
	is.NoErr(e.ds.CreateWebhookEvents(e.ctx, e.dbx, unsubscribed.ID, []int{int(EventBranchTagCreate)}))

	is.NoErr(Deliver(e.ctx, []models.Webhook{deleted, unsubscribed}, samplePayloadFor(e)))

	is.Equal(rec.calls, 0)
	is.Equal(len(e.deliveries(t, unsubscribed.ID)), 0)
}

// endlessServer answers with a 200 and streams a body that never ends, until
// the client goes away.
func endlessServer(t *testing.T) string {
	t.Helper()
	chunk := []byte(strings.Repeat("x", 4096))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		for r.Context().Err() == nil {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// A receiver that streams without end does not hold the sender until its
// timeout: only maxResponseBody bytes are read and stored, marked as cut off,
// and the delivery itself counts as a success.
func TestSendWebhookResponseBodyIsCapped(t *testing.T) {
	is := is.New(t)
	useHTTPClient(t)
	e := newTestEnv(t)
	w := e.newWebhook(t, endlessServer(t), "", ContentTypeJSON, EventPush)

	is.NoErr(SendWebhook(e.ctx, w, EventPush, samplePayloadFor(e)))

	ds := e.deliveries(t, w.ID)
	is.Equal(len(ds), 1)
	is.Equal(ds[0].RequestError.String, "")
	is.Equal(ds[0].ResponseStatus, http.StatusOK)
	is.Equal(ds[0].ResponseBody, strings.Repeat("x", maxResponseBody)+truncatedMarker)
}

// A body of exactly maxResponseBody bytes is stored whole and not marked.
func TestSendWebhookResponseBodyAtLimitIsKept(t *testing.T) {
	is := is.New(t)
	useHTTPClient(t)
	e := newTestEnv(t)
	body := strings.Repeat("y", maxResponseBody)
	srv, _ := newReceiver(t, http.StatusOK, body)
	w := e.newWebhook(t, srv.URL, "", ContentTypeJSON, EventPush)

	is.NoErr(SendWebhook(e.ctx, w, EventPush, samplePayloadFor(e)))

	ds := e.deliveries(t, w.ID)
	is.Equal(len(ds), 1)
	is.Equal(ds[0].ResponseBody, body)
}

// A read that fails before the limit keeps what was read and reports the
// error. A failure after the limit is never reached.
func TestReadResponseBodyKeepsPartialReadOnError(t *testing.T) {
	is := is.New(t)
	boom := errors.New("boom")

	got, err := readResponseBody(io.MultiReader(strings.NewReader("partial"), iotest.ErrReader(boom)))
	is.True(errors.Is(err, boom))
	is.Equal(got, "partial")

	long := strings.Repeat("z", maxResponseBody+10)
	got, err = readResponseBody(io.MultiReader(strings.NewReader(long), iotest.ErrReader(boom)))
	is.NoErr(err)
	is.Equal(got, long[:maxResponseBody]+truncatedMarker)
}
