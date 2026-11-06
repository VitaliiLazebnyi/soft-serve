package testscript

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/rogpeppe/go-internal/txtar"
)

// remoteHost matches a URL-style or scp-style git remote and captures its
// host.
var remoteHost = regexp.MustCompile(`^(?:[a-z][a-z0-9+.-]*://(?:[^@/]*@)?(\[[^\]]*\]|[^:/]*)|[^@:/]+@(\[[^\]]*\]|[^:/]+):)`)

// TestScriptsNeedNoNetwork keeps the testscripts runnable offline: every
// import or git command that is expected to succeed must name a remote on
// loopback, that is the test server itself, never a host on the internet.
func TestScriptsNeedNoNetwork(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "*.txtar"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no testscripts found")
	}
	for _, file := range files {
		ar, err := txtar.ParseFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(ar.Comment), "\n") {
			for _, host := range externalRemotes(line) {
				t.Errorf("%s:%d: %q needs network access to %s; use a repository on the test server instead", file, i+1, strings.TrimSpace(line), host)
			}
		}
	}
}

// externalRemotes returns the non-loopback remote hosts that a script line
// expects to reach: the remotes of a git command or a repo import that is
// not expected to fail.
func externalRemotes(line string) []string {
	line = strings.TrimSpace(line)
	// Drop the [condition] prefixes.
	for strings.HasPrefix(line, "[") {
		end := strings.Index(line, "]")
		if end < 0 {
			return nil
		}
		line = strings.TrimSpace(line[end+1:])
	}
	if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
		return nil
	}
	fields := strings.Fields(line)
	if fields[0] != "git" && !strings.Contains(line, "repo import") {
		return nil
	}
	var hosts []string
	for _, f := range fields[1:] {
		m := remoteHost.FindStringSubmatch(strings.Trim(f, `'"`))
		if m == nil {
			continue
		}
		switch host := strings.Trim(m[1]+m[2], "[]"); host {
		case "localhost", "127.0.0.1", "::1":
		default:
			hosts = append(hosts, host)
		}
	}
	return hosts
}
