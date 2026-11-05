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

// sendWebhook sends a webhook event. When record is false the attempt is not
// written to the deliveries table, and the transport error (if any) is returned
// so the caller can log it.
func sendWebhook(ctx context.Context, w models.Webhook, event Event, payload interface{}, record bool) error {
	var buf bytes.Buffer
	dbx := db.FromContext(ctx)
	datastore := store.FromContext(ctx)

	contentType := ContentType(w.ContentType) //nolint:gosec
	switch contentType {
	case ContentTypeJSON:
		if err := json.NewEncoder(&buf).Encode(payload); err != nil {
			return err
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
			return err
		}
		buf.WriteString(v.Encode()) //nolint: errcheck
	default:
		return ErrInvalidContentType
	}

	headers := http.Header{}
	headers.Add("Content-Type", contentType.String())
	headers.Add("User-Agent", "SoftServe/"+version.Version)
	headers.Add("X-SoftServe-Event", event.String())

	id, err := uuid.NewUUID()
	if err != nil {
		return err
	}

	headers.Add("X-SoftServe-Delivery", id.String())

	reqBody := buf.String()
	if w.Secret != "" {
		sig := hmac.New(sha256.New, []byte(w.Secret))
		sig.Write([]byte(reqBody)) //nolint: errcheck
		headers.Add("X-SoftServe-Signature", "sha256="+hex.EncodeToString(sig.Sum(nil)))
	}

	res, reqErr := do(ctx, w.URL, http.MethodPost, headers, &buf)
	var reqHeaders string
	for k, v := range headers {
		reqHeaders += k + ": " + v[0] + "\n"
	}

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
			b, err := io.ReadAll(res.Body)
			if err != nil {
				reqErr = errors.Join(reqErr, err)
			}

			resBody = string(b)
		}
	}

	if !record {
		return reqErr
	}

	return db.WrapError(datastore.CreateWebhookDelivery(ctx, dbx, id, w.ID, int(event), w.URL, http.MethodPost, reqErr, reqHeaders, reqBody, resStatus, resHeaders, resBody))
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

// Deliver sends the payload to each webhook and records every attempt.
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
	// record, so one bad webhook cannot stop the others from receiving the event.
	var errs []error
	for _, w := range hooks {
		if err := sendWebhook(ctx, w, payload.Event(), payload, record); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
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
