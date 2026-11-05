package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strings"
	"time"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/ssrf"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/utils"
	"github.com/charmbracelet/soft-serve/pkg/version"
	"github.com/google/go-querystring/query"
	"github.com/google/uuid"
)

// Hook is a repository webhook.
type Hook struct {
	models.Webhook
	ContentType ContentType
	Events      []Event
}

// Delivery is a webhook delivery.
type Delivery struct {
	models.WebhookDelivery
	Event Event
}

// secureHTTPClient is an HTTP client with SSRF protection.
var secureHTTPClient = ssrf.NewSecureClient()

// do sends a webhook.
// Caller must close the returned body.
func do(ctx context.Context, url string, method string, headers http.Header, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}

	req.Header = headers
	res, err := secureHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}

	return res, nil
}

// SendWebhook sends a webhook event and records the attempt.
func SendWebhook(ctx context.Context, w models.Webhook, event Event, payload interface{}) error {
	return sendWebhook(ctx, w, event, payload, true)
}

// sendTimeout bounds the request to one webhook, so a slow receiver cannot use
// up the time of the other webhooks an event goes to. It matches the cap of
// the SSRF-protected client. It is a variable so tests can shorten it.
var sendTimeout = ssrf.ClientTimeout

// recordTimeout bounds writing one delivery row.
const recordTimeout = 5 * time.Second

// request is a webhook request ready to send or record.
type request struct {
	id      uuid.UUID
	headers http.Header
	body    string
}

// newRequest encodes payload for w and builds the request headers, including
// the signature when w has a secret.
func newRequest(w models.Webhook, event Event, payload interface{}) (request, error) {
	var buf bytes.Buffer
	contentType := ContentType(w.ContentType) //nolint:gosec
	switch contentType {
	case ContentTypeJSON:
		if err := json.NewEncoder(&buf).Encode(payload); err != nil {
			return request{}, err
		}
	case ContentTypeForm:
		// Redeliveries pass the stored form body as url.Values, which is
		// already encoded; query.Values only accepts structs.
		if v, ok := payload.(neturl.Values); ok {
			buf.WriteString(v.Encode()) //nolint: errcheck
			break
		}
		v, err := query.Values(payload)
		if err != nil {
			return request{}, err
		}
		buf.WriteString(v.Encode()) //nolint: errcheck
	default:
		return request{}, ErrInvalidContentType
	}

	headers := http.Header{}
	headers.Add("Content-Type", contentType.String())
	headers.Add("User-Agent", "SoftServe/"+version.Version)
	headers.Add("X-SoftServe-Event", event.String())

	id, err := uuid.NewUUID()
	if err != nil {
		return request{}, err
	}

	headers.Add("X-SoftServe-Delivery", id.String())

	body := buf.String()
	if w.Secret != "" {
		sig := hmac.New(sha256.New, []byte(w.Secret))
		sig.Write([]byte(body)) //nolint: errcheck
		headers.Add("X-SoftServe-Signature", "sha256="+hex.EncodeToString(sig.Sum(nil)))
	}

	return request{id: id, headers: headers, body: body}, nil
}

// sendWebhook sends a webhook event. The request gets its own sendTimeout.
// When record is false the attempt is not written to the deliveries table,
// and a transport error or a non-2xx response is returned so the caller can
// log it.
func sendWebhook(ctx context.Context, w models.Webhook, event Event, payload interface{}, record bool) error {
	req, err := newRequest(w, event, payload)
	if err != nil {
		return err
	}

	sctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	res, reqErr := do(sctx, w.URL, http.MethodPost, req.headers, strings.NewReader(req.body))

	resStatus := 0
	resHeaders := ""
	resBody := ""

	if res != nil {
		resStatus = res.StatusCode
		for k, v := range res.Header {
			resHeaders += k + ": " + v[0] + "\n"
		}

		if res.Body != nil {
			defer res.Body.Close() //nolint: errcheck
			// A failed read still leaves a delivery record: keep what was
			// read and report the read error with the request error.
			var err error
			resBody, err = readResponseBody(res.Body)
			if err != nil {
				reqErr = errors.Join(reqErr, err)
			}
		}
	}

	if !record {
		if reqErr == nil && (resStatus < 200 || resStatus > 299) {
			return fmt.Errorf("webhook %d: receiver responded with status %d", w.ID, resStatus)
		}
		return reqErr
	}

	return recordDelivery(ctx, w, event, req, reqErr, resStatus, resHeaders, resBody)
}

// maxResponseBody is how much of a receiver's response body is read and
// stored. The rest is not read: the body is closed, so a receiver that streams
// without end cannot hold the sender for its whole timeout or fill the
// deliveries table.
const maxResponseBody = 64 << 10

// truncatedMarker is appended to a stored response body that was cut off at
// maxResponseBody.
const truncatedMarker = "\n[response body truncated by Soft Serve]"

// readResponseBody reads at most maxResponseBody bytes of body. A longer body
// is cut off and marked with truncatedMarker. On a read error it returns what
// was read so far with the error.
func readResponseBody(body io.Reader) (string, error) {
	b, err := io.ReadAll(io.LimitReader(body, maxResponseBody+1))
	if len(b) > maxResponseBody {
		return string(b[:maxResponseBody]) + truncatedMarker, err
	}
	return string(b), err
}

// recordDelivery writes one delivery row. It uses a fresh, short context that
// keeps ctx's values but not its cancellation or deadline, so an attempt that
// ran out of time or was cancelled is still recorded.
func recordDelivery(ctx context.Context, w models.Webhook, event Event, req request, reqErr error, resStatus int, resHeaders, resBody string) error {
	dbx := db.FromContext(ctx)
	datastore := store.FromContext(ctx)

	var reqHeaders string
	for k, v := range req.headers {
		reqHeaders += k + ": " + v[0] + "\n"
	}

	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	return db.WrapError(datastore.CreateWebhookDelivery(rctx, dbx, req.id, w.ID, int(event), w.URL, http.MethodPost, reqErr, reqHeaders, req.body, resStatus, resHeaders, resBody))
}

// RecordUndelivered records a delivery of payload to each webhook without
// sending it, with reason as the request error. Use it when an event cannot
// be sent at all; the recorded request can be redelivered later.
func RecordUndelivered(ctx context.Context, hooks []models.Webhook, payload EventPayload, reason error) error {
	var errs []error
	for _, w := range hooks {
		req, err := newRequest(w, payload.Event(), payload)
		if err == nil {
			err = recordDelivery(ctx, w, payload.Event(), req, reason, 0, "", "")
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Subscribers returns the webhooks of the payload's repository that subscribe
// to its event. Call it before a change that removes those webhooks, since
// they cannot be found afterwards.
func Subscribers(ctx context.Context, payload EventPayload) ([]models.Webhook, error) {
	dbx := db.FromContext(ctx)
	datastore := store.FromContext(ctx)
	webhooks, err := datastore.GetWebhooksByRepoIDWhereEvent(ctx, dbx, payload.RepositoryID(), []int{int(payload.Event())})
	if err != nil {
		return nil, db.WrapError(err)
	}
	return webhooks, nil
}

// Deliver sends the payload to each webhook and records every attempt. Each
// webhook is re-read just before its send: one deleted or unsubscribed since
// hooks was read is skipped, and a changed URL or secret is used.
func Deliver(ctx context.Context, hooks []models.Webhook, payload EventPayload) error {
	return deliver(ctx, hooks, payload, true)
}

// DeliverUnrecorded sends the payload to each webhook without recording the
// attempts. Use it when the webhooks are being removed, so no delivery row is
// written that refers to a webhook that no longer exists.
func DeliverUnrecorded(ctx context.Context, hooks []models.Webhook, payload EventPayload) error {
	return deliver(ctx, hooks, payload, false)
}

func deliver(ctx context.Context, hooks []models.Webhook, payload EventPayload, record bool) error {
	// Every webhook is attempted even if an earlier one fails to send or
	// record, so one bad webhook cannot stop the others from receiving the
	// event. Each send has its own timeout, so a slow receiver does not use up
	// the time of the webhooks after it.
	var errs []error
	for _, w := range hooks {
		if record {
			var ok bool
			if w, ok = currentHook(ctx, w, payload.Event()); !ok {
				continue
			}
		}
		if err := sendWebhook(ctx, w, payload.Event(), payload, record); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// currentHook re-reads w right before it is sent, since a delivery can wait in
// the background queue while its webhook is changed. It returns the webhook as
// stored now, so a rotated secret or changed URL is used, and false when the
// webhook was deleted or no longer subscribes to event. If the webhook cannot
// be read, w is returned unchanged and the send goes ahead as before, with its
// outcome recorded as usual.
func currentHook(ctx context.Context, w models.Webhook, event Event) (models.Webhook, bool) {
	dbx := db.FromContext(ctx)
	datastore := store.FromContext(ctx)
	hooks, err := datastore.GetWebhooksByRepoIDWhereEvent(ctx, dbx, w.RepoID, []int{int(event)})
	if err != nil {
		return w, true
	}
	for _, h := range hooks {
		if h.ID == w.ID {
			return h, true
		}
	}
	return models.Webhook{}, false
}

// SendEvent sends a webhook event to the repository's subscribers.
func SendEvent(ctx context.Context, payload EventPayload) error {
	hooks, err := Subscribers(ctx, payload)
	if err != nil {
		return err
	}
	return Deliver(ctx, hooks, payload)
}

func repoURL(publicURL string, repo string) string {
	return fmt.Sprintf("%s/%s.git", publicURL, utils.SanitizeRepo(repo))
}

func getDefaultBranch(repo proto.Repository) (string, error) {
	branch, err := proto.RepositoryDefaultBranch(repo)
	// XXX: we check for ErrReferenceNotExist here because we don't want to
	// return an error if the repo is an empty repo.
	// This means that the repo doesn't have a default branch yet and this is
	// the first push to it.
	if err != nil && !errors.Is(err, git.ErrReferenceNotExist) {
		return "", err
	}

	return branch, nil
}
