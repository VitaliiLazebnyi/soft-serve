package web

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"charm.land/log/v2"
	"github.com/matryer/is"
)

// Authentication logs never carry a credential: not the Authorization header,
// not an access token sent as the username or password, and not a password.
//
// Backend lookups log through the logger captured when the backend is
// created, which falls back to the global default, so the test swaps the
// global default logger and must not run in parallel.
func TestAuthDoesNotLogCredentials(t *testing.T) {
	is := is.New(t)
	var logs bytes.Buffer
	logger := log.New(&logs)
	logger.SetLevel(log.DebugLevel)
	prev := log.Default()
	log.SetDefault(logger)
	t.Cleanup(func() { log.SetDefault(prev) })

	f := newOracleFixture(t)
	ctx := log.WithContext(f.ctx, logger)
	is.NoErr(f.be.SetPassword(ctx, "stranger", "hunter2-password"))
	stranger, err := f.be.User(ctx, "stranger")
	is.NoErr(err)
	jwt := mintJWT(ctx, t, stranger, "secret")

	const (
		badToken = "ss_bad-token-do-not-log"
		badPass  = "bad-password-do-not-log"
	)
	basic := func(user, pass string) string {
		r := httptest.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
		r.SetBasicAuth(user, pass)
		return r.Header.Get("Authorization")
	}

	for _, tc := range []struct {
		name   string
		header string
		ok     bool
		scheme string
		// secrets must not appear in the logs.
		secrets []string
	}{
		{"token", "Token " + f.token, true, "token", []string{f.token}},
		{"bad token", "token " + badToken, false, "token", []string{badToken}},
		{"bearer", "Bearer " + jwt, true, "bearer", []string{jwt}},
		{"bad bearer", "Bearer " + badToken, false, "bearer", []string{badToken}},
		{"basic password", basic("stranger", "hunter2-password"), true, "basic", []string{"hunter2-password"}},
		{"basic bad password", basic("stranger", badPass), false, "basic", []string{badPass}},
		{"basic token as password", basic("stranger", f.token), true, "basic", []string{f.token}},
		{"basic token as username", basic(f.token, ""), true, "basic", []string{f.token}},
		{"basic bad token as username", basic(badToken, ""), false, "basic", []string{badToken}},
		// GitHub-style clients send the token as the username with a
		// placeholder password; both lookups fail.
		{"basic token with placeholder", basic(badToken, "x-oauth-basic"), false, "basic", []string{badToken}},
		// A malformed header whose first word is the credential.
		{"malformed", badToken + " x", false, "other", []string{badToken}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			is := is.New(t)
			logs.Reset()
			req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
			req.Header.Set("Authorization", tc.header)

			user, err := parseAuthHdr(req, "secret")
			is.Equal(err == nil && user != nil, tc.ok)

			out := logs.String()
			for _, s := range tc.secrets {
				if strings.Contains(out, s) {
					t.Errorf("auth log contains credential %q:\n%s", s, out)
				}
			}
			// Whatever follows a known scheme, such as the base64 Basic
			// payload, is a credential too.
			if _, cred, _ := strings.Cut(tc.header, " "); tc.scheme != "other" && strings.Contains(out, cred) {
				t.Errorf("auth log contains the header credential:\n%s", out)
			}
			is.True(strings.Contains(out, "scheme="+tc.scheme))
		})
	}
}
