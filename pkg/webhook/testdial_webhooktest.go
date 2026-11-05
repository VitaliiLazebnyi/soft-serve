//go:build webhooktest

package webhook

import (
	"context"
	"net"
	"net/http"
	"os"

	"github.com/charmbracelet/soft-serve/pkg/ssrf"
)

// This file is compiled only with the webhooktest build tag, which the
// testscript suite uses to build its soft binary. Release builds never contain
// it.
//
// When SOFT_SERVE_TEST_WEBHOOK_ADDR is set, every webhook request is dialed to
// that address instead of the webhook's host. Webhook URLs are still validated
// as usual, so the tests use public addresses, but nothing leaves the machine:
// the requests reach a receiver the test runs on loopback.
func init() {
	addr := os.Getenv("SOFT_SERVE_TEST_WEBHOOK_ADDR")
	if addr == "" {
		return
	}
	secureHTTPClient = &http.Client{
		Timeout: ssrf.ClientTimeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, network, addr)
			},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
