package lfs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/ssrf"
)

// httpClient is a Git LFS client to communicate with a LFS source API.
type httpClient struct {
	client    *http.Client
	endpoint  Endpoint
	transfers map[string]TransferAdapter
}

var _ Client = (*httpClient)(nil)

// stallTimeout is how long an LFS connection may go without sending or
// receiving any data before it is closed. Tests lower it.
var stallTimeout = ssrf.ClientTimeout

// secureHTTPClient is the SSRF-protected HTTP client shared by every LFS
// client, so they all draw from one connection pool instead of each keeping
// its own idle connections. Its transfer timeouts are set once here, as
// setTransferTimeouts wraps the shared transport's dialer.
var secureHTTPClient = newTransferClient(ssrf.NewSecureClient())

// newTransferClient sets up client for LFS transfers and returns it. Tests
// pass a client that may reach loopback, so the stall tests run through the
// same setup as secureHTTPClient.
func newTransferClient(client *http.Client) *http.Client {
	setTransferTimeouts(client)
	return client
}

// newHTTPClient returns a new Git LFS client.
func newHTTPClient(endpoint Endpoint) *httpClient {
	client := secureHTTPClient
	return &httpClient{
		client:   client,
		endpoint: endpoint,
		transfers: map[string]TransferAdapter{
			TransferBasic: &BasicTransferAdapter{client},
		},
	}
}

// setTransferTimeouts replaces the client's whole-request timeout, which also
// covers reading the response body and so cuts off any object that takes
// longer than it to transfer, with an idle timeout on each connection: a
// transfer may take as long as it needs while data keeps moving, and a stalled
// one still fails. It panics if the client's transport is not an
// *http.Transport with a DialContext, since it could not add the stall limit
// and transfers would then have no bound at all.
func setTransferTimeouts(client *http.Client) {
	t, ok := client.Transport.(*http.Transport)
	if !ok || t.DialContext == nil {
		panic(fmt.Sprintf("lfs: cannot add a stall timeout to transport %T", client.Transport))
	}
	client.Timeout = 0
	dial := t.DialContext
	t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &stallTimeoutConn{Conn: conn, timeout: stallTimeout}, nil
	}
}

// stallTimeoutConn pushes the connection's deadline forward on every read and
// write, so it only expires once neither direction has made progress for the
// timeout. Setting the deadline from either side also extends a read already
// waiting, such as the transport's wait for a response while a large upload
// is still being written.
type stallTimeoutConn struct {
	net.Conn
	timeout time.Duration
}

func (c *stallTimeoutConn) Read(b []byte) (int, error) {
	if err := c.Conn.SetDeadline(time.Now().Add(c.timeout)); err != nil {
		return 0, err //nolint:wrapcheck
	}
	return c.Conn.Read(b) //nolint:wrapcheck
}

func (c *stallTimeoutConn) Write(b []byte) (int, error) {
	if err := c.Conn.SetDeadline(time.Now().Add(c.timeout)); err != nil {
		return 0, err //nolint:wrapcheck
	}
	return c.Conn.Write(b) //nolint:wrapcheck
}

// Download implements Client.
func (c *httpClient) Download(ctx context.Context, objects []Pointer, callback DownloadCallback) error {
	return c.performOperation(ctx, objects, callback, nil)
}

// Upload implements Client.
func (c *httpClient) Upload(ctx context.Context, objects []Pointer, callback UploadCallback) error {
	return c.performOperation(ctx, objects, nil, callback)
}

func (c *httpClient) transferNames() []string {
	names := make([]string, len(c.transfers))
	i := 0
	for name := range c.transfers {
		names[i] = name
		i++
	}
	return names
}

// batch performs a batch request to the LFS server.
func (c *httpClient) batch(ctx context.Context, operation string, objects []Pointer) (*BatchResponse, error) {
	logger := log.FromContext(ctx).WithPrefix("lfs")
	url := fmt.Sprintf("%s/objects/batch", c.endpoint.String())

	// TODO: support ref
	request := &BatchRequest{operation, c.transferNames(), nil, objects, HashAlgorithmSHA256}

	payload := new(bytes.Buffer)
	err := json.NewEncoder(payload).Encode(request)
	if err != nil {
		logger.Errorf("Error encoding json: %v", err)
		return nil, err
	}

	logger.Debugf("Calling: %s", url)

	req, err := http.NewRequestWithContext(ctx, "POST", url, payload)
	if err != nil {
		logger.Errorf("Error creating request: %v", err)
		return nil, err
	}
	req.Header.Set("Content-type", MediaType)
	req.Header.Set("Accept", MediaType)

	res, err := c.client.Do(req)
	if err != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		logger.Errorf("Error while processing request: %v", err)
		return nil, err
	}
	defer res.Body.Close() //nolint: errcheck

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected server response: %s", res.Status)
	}

	var response BatchResponse
	err = json.NewDecoder(res.Body).Decode(&response)
	if err != nil {
		logger.Errorf("Error decoding json: %v", err)
		return nil, err
	}

	if len(response.Transfer) == 0 {
		response.Transfer = TransferBasic
	}

	return &response, nil
}

func (c *httpClient) performOperation(ctx context.Context, objects []Pointer, dc DownloadCallback, uc UploadCallback) error {
	logger := log.FromContext(ctx).WithPrefix("lfs")
	if len(objects) == 0 {
		return nil
	}

	operation := OperationDownload
	if uc != nil {
		operation = OperationUpload
	}

	result, err := c.batch(ctx, operation, objects)
	if err != nil {
		return err
	}

	transferAdapter, ok := c.transfers[result.Transfer]
	if !ok {
		return fmt.Errorf("TransferAdapter not found: %s", result.Transfer)
	}

	for _, object := range result.Objects {
		if object.Error != nil {
			objectError := errors.New(object.Error.Message)
			logger.Debugf("Error on object %v: %v", object.Pointer, objectError)
			if uc != nil {
				if _, err := uc(object.Pointer, objectError); err != nil {
					return err
				}
			} else {
				if err := dc(object.Pointer, nil, objectError); err != nil {
					return err
				}
			}
			continue
		}

		if uc != nil {
			if len(object.Actions) == 0 {
				logger.Debugf("%v already present on server", object.Pointer)
				continue
			}

			link, ok := object.Actions[ActionUpload]
			if !ok {
				logger.Debugf("%+v", object)
				return errors.New("missing action 'upload'")
			}

			content, err := uc(object.Pointer, nil)
			if err != nil {
				return err
			}

			err = transferAdapter.Upload(ctx, object.Pointer, content, link)

			content.Close() //nolint: errcheck

			if err != nil {
				return err
			}

			link, ok = object.Actions[ActionVerify]
			if ok {
				if err := transferAdapter.Verify(ctx, object.Pointer, link); err != nil {
					return err
				}
			}
		} else {
			link, ok := object.Actions[ActionDownload]
			if !ok {
				logger.Debugf("%+v", object)
				return errors.New("missing action 'download'")
			}

			content, err := transferAdapter.Download(ctx, object.Pointer, link)
			if err != nil {
				return err
			}

			if err := dc(object.Pointer, content, nil); err != nil {
				return err
			}
		}
	}

	return nil
}
