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

	"charm.land/log/v2"
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

// ErrWebhookInactive is returned when a delivery is redelivered to a webhook
// that is not active. Activate the webhook first to send to it again.
var ErrWebhookInactive = errors.New("webhook is not active")

// secureHTTPClient is an HTTP client with SSRF protection.
var secureHTTPClient = ssrf.NewSecureClient()

// do sends a webhook.
// Caller must close the returned body.
// A returned error never contains the webhook URL's path, query, fragment or
// user info, which can hold secrets (see redactURLError).
func do(ctx context.Context, url string, method string, headers http.Header, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, redactURLError(err)
	}

	req.Header = headers
	res, err := secureHTTPClient.Do(req)
	if err != nil {
		return nil, redactURLError(err)
	}

	return res, nil
}

// redactedURLPart replaces the parts of a webhook URL that are left out of
// errors.
const redactedURLPart = "[redacted]"

// redactURL returns rawURL reduced to its scheme and host. Many receivers
// (Slack, Discord, Teams and the like) put a token in the path or query, so
// those parts, the fragment and any user info are replaced with
// redactedURLPart. A URL that cannot be parsed is replaced entirely.
//
// The host is kept on purpose, so an operator can tell which receiver failed.
// A receiver that puts a token in the host name (a per-hook subdomain such as
// https://<id>.m.pipedream.net) therefore still has that token in the error.
func redactURL(rawURL string) string {
	u, err := neturl.Parse(rawURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return redactedURLPart
	}
	redacted := u.Scheme + "://" + u.Host
	if u.Opaque != "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		redacted += "/" + redactedURLPart
	}
	return redacted
}

// errInvalidURL replaces the parser's message when a webhook URL cannot be
// parsed.
var errInvalidURL = errors.New("invalid URL")

// redactURLError redacts the URL of a *url.Error, which the HTTP client
// returns for every failed request and which prints the whole URL except for
// a password. The returned error keeps the operation and the underlying
// error (except for a parse error, whose message is replaced), so errors.Is
// and errors.As (context.DeadlineExceeded, net.Error timeouts and so on)
// still work. Any other error is returned unchanged.
func redactURLError(err error) error {
	uerr, ok := err.(*neturl.Error) //nolint:errorlint // only the top-level error is replaced
	if !ok {
		return err
	}
	inner := uerr.Err
	if uerr.Op == "parse" {
		// A parse error's message can quote part of the URL, such as an
		// invalid escape or port.
		inner = errInvalidURL
	}
	return &neturl.Error{Op: uerr.Op, URL: redactURL(uerr.URL), Err: inner}
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

// rereadTimeout bounds each try at re-reading a webhook before its send, so a
// database that does not answer cannot hold a background sender. It is a
// variable so tests can shorten it.
var rereadTimeout = recordTimeout

// rereadRetryDelay is the wait before the one retry of a failed re-read. It is
// a variable so tests can shorten it.
var rereadRetryDelay = 200 * time.Millisecond

// errRereadFailed is the reason stored on a delivery that was not sent because
// its webhook could not be re-read. The database error itself is only logged:
// the stored reason is shown to repository admins, who need not see database
// details.
var errRereadFailed = errors.New("not sent: could not re-read webhook (database error)")

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
		if reqErr != nil {
			// The URL in reqErr is redacted, so name the webhook: several
			// hooks on one host would otherwise log the same line.
			return fmt.Errorf("webhook %d: %w", w.ID, reqErr)
		}
		return nil
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
	create := func(h db.Handler) error {
		return datastore.CreateWebhookDelivery(rctx, h, req.id, w.ID, int(event), w.URL, http.MethodPost, reqErr, reqHeaders, req.body, resStatus, resHeaders, resBody)
	}

	// The delivery is recorded and the older ones pruned in one transaction,
	// so SQLite takes its write lock and syncs once. Recording comes first:
	// a SQLite transaction that writes first waits for other writers rather
	// than failing with SQLITE_BUSY.
	//
	// On Postgres seq is taken from its sequence when a row is inserted, not
	// when it commits. Deliveries of one webhook are recorded by separate
	// processes (the server and each git hook), so a delivery whose
	// transaction commits late can already be older, by seq, than the
	// newest maxDeliveries rows and be pruned by the next record. That needs
	// maxDeliveries deliveries of the same webhook recorded while its insert
	// is in flight; the cost is one delivery missing from the history, which
	// pruning by seq accepts rather than ordering commits.
	var pruneErr error
	err := dbx.TransactionContext(rctx, func(tx *db.Tx) error {
		if err := create(tx); err != nil {
			return err
		}
		pruneErr = datastore.PruneWebhookDeliveries(rctx, tx, w.ID, maxDeliveries)
		return pruneErr
	})
	if err != nil && pruneErr != nil {
		// A failed prune only leaves extra rows for the next delivery's
		// prune, so it must not lose the delivery: the transaction was
		// rolled back, and the delivery is recorded again without pruning.
		log.FromContext(ctx).Warn("pruning webhook deliveries", "webhook", w.ID, "err", pruneErr)
		err = create(dbx)
	}
	return db.WrapError(err)
}

// maxDeliveries is how many deliveries are kept per webhook. Every recorded
// delivery deletes the older ones, so events, including ones recorded without
// sending when the queue is full, cannot grow the deliveries table without
// limit. Migration 5 (pkg/db/migrate/0005_webhook_delivery_retention_*.up.sql)
// trimmed existing tables once to the then value, 100; it is a snapshot and
// does not follow later changes here. It is a variable so tests can lower it.
var maxDeliveries = 100

// MaxWebhooksPerRepo is how many webhooks a repository can have. Each event
// is sent to, recorded for and pruned for every subscribed webhook, and each
// webhook keeps maxDeliveries deliveries, so this bounds both the work one
// event causes and the deliveries a repository stores.
const MaxWebhooksPerRepo = 20

// ErrTooManyWebhooks is returned when creating a webhook for a repository
// that already has MaxWebhooksPerRepo webhooks.
var ErrTooManyWebhooks = fmt.Errorf("a repository can have at most %d webhooks", MaxWebhooksPerRepo)

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

// Subscribers returns the active webhooks of the payload's repository that
// subscribe to its event. Inactive webhooks are not sent events. Call it
// before a change that removes those webhooks, since they cannot be found
// afterwards.
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
// hooks was read is skipped, as is one deactivated since, and a changed URL or
// secret is used. A webhook that cannot be re-read is not sent to: the event
// is recorded for it as undelivered, so it can be redelivered later, and a
// warning with the error is logged.
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
			cur, ok, err := currentHook(ctx, w, payload.Event())
			if err != nil {
				// Whether w may still be sent to is unknown, so it is not
				// sent to. The event is recorded as undelivered instead, so
				// it can be redelivered once the webhook can be read. The
				// error is logged here, since a recorded delivery is not
				// returned as an error; the stored reason leaves out the
				// database details. Only a failure to record is returned.
				log.FromContext(ctx).WithPrefix("webhook").Warn("webhook not sent: re-reading it failed; recorded as undelivered",
					"webhook_id", w.ID, "repo_id", w.RepoID, "event", payload.Event(), "err", err)
				if err := RecordUndelivered(ctx, []models.Webhook{w}, payload, errRereadFailed); err != nil {
					errs = append(errs, err)
				}
				continue
			}
			if !ok {
				continue
			}
			w = cur
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
// webhook was deleted, deactivated, or no longer subscribes to event. It
// returns an error when the webhook cannot be read; the caller then does not
// send, since the webhook may have been deactivated.
//
// Each try is bounded by rereadTimeout, and a failed try is retried once after
// rereadRetryDelay unless ctx is done, so a short database hiccup does not
// leave the event undelivered.
func currentHook(ctx context.Context, w models.Webhook, event Event) (models.Webhook, bool, error) {
	hooks, err := lookupSubscribers(ctx, w.RepoID, event)
	if err != nil && ctx.Err() == nil {
		select {
		case <-ctx.Done():
		case <-time.After(rereadRetryDelay):
			hooks, err = lookupSubscribers(ctx, w.RepoID, event)
		}
	}
	if err != nil {
		return models.Webhook{}, false, err
	}
	for _, h := range hooks {
		if h.ID == w.ID {
			return h, true, nil
		}
	}
	return models.Webhook{}, false, nil
}

// lookupSubscribers reads the active webhooks of a repository that subscribe
// to event, giving up after rereadTimeout.
func lookupSubscribers(ctx context.Context, repoID int64, event Event) ([]models.Webhook, error) {
	lctx, cancel := context.WithTimeout(ctx, rereadTimeout)
	defer cancel()
	hooks, err := store.FromContext(ctx).GetWebhooksByRepoIDWhereEvent(lctx, db.FromContext(ctx), repoID, []int{int(event)})
	if err != nil {
		return nil, db.WrapError(err)
	}
	return hooks, nil
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
