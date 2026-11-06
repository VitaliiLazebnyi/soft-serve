package web

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
		badJWT   = "eyJhbGciOiJFZERTQSJ9.eyJzdWIiOiJ4In0.bad-sig-do-not-log"
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
		// logged must appear in the logs.
		logged []string
	}{
		{"token", "Token " + f.token, true, "token", []string{f.token}, nil},
		{"bad token", "token " + badToken, false, "token", []string{badToken}, nil},
		{"bearer", "Bearer " + jwt, true, "bearer", []string{jwt}, nil},
		{"bad bearer", "Bearer " + badToken, false, "bearer", []string{badToken}, nil},
		{"basic password", basic("stranger", "hunter2-password"), true, "basic", []string{"hunter2-password"}, nil},
		// A valid username is logged on failure, for per-account monitoring.
		{"basic bad password", basic("stranger", badPass), false, "basic", []string{badPass}, []string{"username=stranger"}},
		{"basic token as password", basic("stranger", f.token), true, "basic", []string{f.token}, nil},
		{"basic token as username", basic(f.token, ""), true, "basic", []string{f.token}, nil},
		{"basic bad token as username", basic(badToken, ""), false, "basic", []string{badToken}, nil},
		// GitHub-style clients send the token as the username with a
		// placeholder password; both lookups fail.
		{"basic token with placeholder", basic(badToken, "x-oauth-basic"), false, "basic", []string{badToken}, nil},
		{"basic jwt with placeholder", basic(badJWT, "x-oauth-basic"), false, "basic", []string{badJWT}, nil},
		// A malformed header whose first word is the credential.
		{"malformed", badToken + " x", false, "other", []string{badToken}, nil},
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
			for _, s := range tc.logged {
				if !strings.Contains(out, s) {
					t.Errorf("auth log does not contain %q:\n%s", s, out)
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

// A failed login is the client's mistake, not a server error, so none of
// them is logged at error level: anyone could otherwise flood the error log.
// Like TestAuthDoesNotLogCredentials, this swaps the global default logger
// and must not run in parallel.
func TestFailedAuthNotLoggedAsError(t *testing.T) {
	is := is.New(t)
	var logs bytes.Buffer
	logger := log.New(&logs)
	logger.SetLevel(log.DebugLevel)
	prev := log.Default()
	log.SetDefault(logger)
	t.Cleanup(func() { log.SetDefault(prev) })

	f := newOracleFixture(t)
	ctx := log.WithContext(f.ctx, logger)
	stranger, err := f.be.User(ctx, "stranger")
	is.NoErr(err)
	expired, err := f.be.CreateAccessToken(ctx, stranger, "old", time.Now().Add(-time.Hour))
	is.NoErr(err)

	basic := func(user, pass string) string {
		r := httptest.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
		r.SetBasicAuth(user, pass)
		return r.Header.Get("Authorization")
	}

	for name, header := range map[string]string{
		"bad token":                   "token ss_bad-token",
		"expired token":               "token " + expired,
		"bad bearer":                  "Bearer ss_bad-token",
		"basic bad password":          basic("stranger", "bad-password"),
		"basic expired token as pass": basic("stranger", expired),
		"basic bad token as username": basic("ss_bad-token", ""),
		"basic expired as username":   basic(expired, ""),
	} {
		t.Run(name, func(t *testing.T) {
			is := is.New(t)
			logs.Reset()
			req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
			req.Header.Set("Authorization", header)

			user, err := parseAuthHdr(req, "secret")
			is.True(err != nil || user == nil)

			if out := logs.String(); strings.Contains(out, "ERRO") {
				t.Errorf("failed login logged at error level:\n%s", out)
			}
		})
	}
}
