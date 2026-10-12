package updatecheck_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/updatecheck"
)

const (
	latestPath   = "/repos/ghostbranch-labs/ghostbranch/releases/latest"
	advisoryPath = "/repos/ghostbranch-labs/ghostbranch/security-advisories"
)

type ghServer struct {
	mu      sync.Mutex
	headers []http.Header
	queries []string
	paths   []string
}

// newGH serves latest and advisories as given; a status other than 200
// answers that status with an empty body.
func newGH(t *testing.T, latestStatus int, latest string, advisories string) (*ghServer, updatecheck.Client) {
	t.Helper()
	g := &ghServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.headers = append(g.headers, r.Header.Clone())
		g.queries = append(g.queries, r.URL.RawQuery)
		g.paths = append(g.paths, r.URL.Path)
		g.mu.Unlock()
		switch r.URL.Path {
		case latestPath:
			if latestStatus != http.StatusOK {
				w.WriteHeader(latestStatus)
				return
			}
			fmt.Fprint(w, latest)
		case advisoryPath:
			fmt.Fprint(w, advisories)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return g, updatecheck.Client{BaseURL: srv.URL, HTTP: updatecheck.NewHTTPClient()}
}

func clock() time.Time { return time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC) }

func release(tag string) string {
	return `{"tag_name":"` + tag + `","draft":false,"prerelease":false,"html_url":"https://evil.example/x"}`
}

const advisoryJSON = `[
 {"ghsa_id":"GHSA-2222-3333-4444","summary":"Leak gate skips a file","withdrawn_at":null,
  "vulnerabilities":[{"package":{"ecosystem":"go","name":"github.com/ghostbranch-labs/ghostbranch"},"vulnerable_version_range":"< 0.13.1","patched_versions":"0.13.1"}]},
 {"ghsa_id":"GHSA-5555-6666-7777","summary":"Old issue","withdrawn_at":null,
  "vulnerabilities":[{"vulnerable_version_range":"< 0.10.0","patched_versions":"0.10.0"}]},
 {"ghsa_id":"GHSA-8888-9999-cccc","summary":"Odd range","withdrawn_at":null,
  "vulnerabilities":[{"vulnerable_version_range":"~> 0.13","patched_versions":""}]},
 {"ghsa_id":"GHSA-gggg-hhhh-jjjj","summary":"Withdrawn","withdrawn_at":"2026-10-01T00:00:00Z",
  "vulnerabilities":[{"vulnerable_version_range":"< 9.0.0","patched_versions":""}]}
]`

func TestLookNewerReleaseWithAdvisories(t *testing.T) {
	t.Parallel()
	g, c := newGH(t, http.StatusOK, release("v0.13.1"), advisoryJSON)
	r := updatecheck.Look(context.Background(), c, mustParse(t, "v0.13.0"), clock, time.Second)
	if r.Err != nil || r.Latest == nil || r.Latest.Version.String() != "0.13.1" || !r.At.Equal(clock()) {
		t.Fatalf("look = %+v", r)
	}
	// Built locally from the tag, never the server's html_url.
	if r.Latest.URL != "https://github.com/ghostbranch-labs/ghostbranch/releases/tag/v0.13.1" {
		t.Fatalf("url %q", r.Latest.URL)
	}
	var ids []string
	for _, a := range r.Advisories {
		ids = append(ids, a.ID)
	}
	// Inside the range, and the unreadable range (fail toward warning); not
	// the old one, not the withdrawn one.
	if strings.Join(ids, ",") != "GHSA-2222-3333-4444,GHSA-8888-9999-cccc" {
		t.Fatalf("advisories %v", ids)
	}
	if a := r.Advisories[0]; a.Range != "< 0.13.1" || a.Patched != "0.13.1" || a.Summary != "Leak gate skips a file" {
		t.Fatalf("advisory %+v", a)
	}
	if !strings.Contains(g.queries[1], "state=published") || !strings.Contains(g.queries[1], "per_page=100") {
		t.Fatalf("advisory query %q", g.queries[1])
	}
}

// Same or older release: the advisories aren't asked for.
func TestLookSameOrOlderAsksOnce(t *testing.T) {
	t.Parallel()
	for _, tag := range []string{"v0.13.0", "v0.12.9"} {
		g, c := newGH(t, http.StatusOK, release(tag), advisoryJSON)
		r := updatecheck.Look(context.Background(), c, mustParse(t, "v0.13.0"), clock, time.Second)
		if r.Err != nil || r.Latest == nil || len(r.Advisories) != 0 || len(g.paths) != 1 {
			t.Fatalf("%s: %+v, %d requests", tag, r, len(g.paths))
		}
	}
}

func TestLookFailures(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		status int
		body   string
		want   string
	}{
		{http.StatusNotFound, "", "no release published yet"},
		{http.StatusForbidden, "", "rate limit"},
		{http.StatusTooManyRequests, "", "rate limit"},
		{http.StatusInternalServerError, "", "HTTP 500"},
		{http.StatusOK, "{not json", "read the latest release"},
		{http.StatusOK, release("nightly"), `isn't a version`},
		{http.StatusOK, `{"tag_name":"v0.14.0","prerelease":true}`, `isn't a version`},
	} {
		_, cl := newGH(t, c.status, c.body, "[]")
		r := updatecheck.Look(context.Background(), cl, mustParse(t, "v0.13.0"), clock, time.Second)
		if r.Err == nil || !strings.Contains(r.Err.Error(), c.want) || r.Latest != nil {
			t.Errorf("%d %q: %+v", c.status, c.body, r)
		}
	}
	_, cl := newGH(t, http.StatusNotFound, "", "[]")
	if r := updatecheck.Look(context.Background(), cl, mustParse(t, "v0.13.0"), clock, time.Second); !errors.Is(r.Err, updatecheck.ErrNoRelease) {
		t.Fatalf("404 must be ErrNoRelease: %v", r.Err)
	}
}

// Review Focus 2: a server slower than the budget is given up on.
func TestLookGivesUpAtTheBudget(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)
	c := updatecheck.Client{BaseURL: srv.URL, HTTP: updatecheck.NewHTTPClient()}
	start := time.Now()
	r := updatecheck.Look(context.Background(), c, mustParse(t, "v0.13.0"), clock, 200*time.Millisecond)
	if r.Err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("look = %+v after %s", r, time.Since(start))
	}
}

// No token, no IDs: the requests carry no Authorization and the plain
// User-Agent.
func TestLookIsAnonymous(t *testing.T) {
	t.Parallel()
	g, c := newGH(t, http.StatusOK, release("v0.13.1"), advisoryJSON)
	updatecheck.Look(context.Background(), c, mustParse(t, "v0.13.0"), clock, time.Second)
	if len(g.headers) != 2 {
		t.Fatalf("%d requests", len(g.headers))
	}
	for _, h := range g.headers {
		if h.Get("Authorization") != "" || h.Get("User-Agent") != "ghostbranch" || h.Get("Cookie") != "" {
			t.Fatalf("headers %v", h)
		}
	}
}

// Review Focus 1: text from the network is stripped of control characters.
func TestNetworkTextIsCleaned(t *testing.T) {
	t.Parallel()
	adv := `[{"ghsa_id":"GHSA-2222-3333-4444\u001b[2J","summary":"bad\u001b]0;title\u0007\nline","withdrawn_at":null,
	  "vulnerabilities":[{"vulnerable_version_range":"< 0.13.1","patched_versions":"0.13.1\r"}]}]`
	_, c := newGH(t, http.StatusOK, release("v0.13.1"), adv)
	r := updatecheck.Look(context.Background(), c, mustParse(t, "v0.13.0"), clock, time.Second)
	if len(r.Advisories) != 1 {
		t.Fatalf("%+v", r)
	}
	a := r.Advisories[0]
	for _, s := range []string{a.ID, a.Summary, a.Patched} {
		if strings.ContainsAny(s, "\x1b\x07\r\n") {
			t.Fatalf("control character left in %q", s)
		}
	}
}
