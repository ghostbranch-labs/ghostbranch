package updatecheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"
)

const (
	// Repo is where releases and advisories are read. A rename at launch
	// changes this one line.
	Repo = "ghostbranch-labs/ghostbranch"
	// DefaultBaseURL is GitHub's API.
	DefaultBaseURL = "https://api.github.com"
	// Budget caps both requests of one look together.
	Budget = 3 * time.Second
	// userAgent is the whole User-Agent: no version, no OS.
	userAgent = "ghostbranch"
	// releaseCap and advisoriesCap bound what a response may make us read.
	// The advisory list gets more room so a long list can't be cut short and
	// lose the one warning that matters.
	releaseCap    = 1 << 20
	advisoriesCap = 4 << 20
)

// ErrNoRelease means GitHub has no published release to show: before launch
// the repo is private and answers 404.
var ErrNoRelease = errors.New("no release published yet")

// Client reads releases and advisories anonymously. It never carries a
// token, a cookie or an ID.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// NewHTTPClient is a client with Go's default transport settings, which
// honour HTTPS_PROXY, HTTP_PROXY and NO_PROXY, and no cookie jar. A look
// that started on https never follows a redirect to anything else.
func NewHTTPClient() *http.Client {
	// Something may have replaced http.DefaultTransport; a look runs in the
	// background, where a panic would kill the command, so fall back to a
	// fresh transport that still honours the proxy settings.
	var t *http.Transport
	if dt, ok := http.DefaultTransport.(*http.Transport); ok {
		t = dt.Clone()
	} else {
		t = &http.Transport{Proxy: http.ProxyFromEnvironment}
	}
	return &http.Client{Transport: t, CheckRedirect: noDowngrade}
}

func noDowngrade(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
		return errors.New("refused a redirect away from https")
	}
	return nil
}

// Release is the newest published, non-draft, non-prerelease release.
type Release struct {
	Version Version
	Tag     string
	URL     string
}

// Advisory is a published GitHub security advisory that affects this
// binary's version.
type Advisory struct {
	ID      string `json:"id"`
	Summary string `json:"summary"`
	Range   string `json:"range"`
	Patched string `json:"patched"`
}

// ReleaseURL is a release's page, built here from a tag that parsed as a
// version: the server's html_url is never shown.
func ReleaseURL(tag string) string { return "https://github.com/" + Repo + "/releases/tag/" + tag }

type statusError int

func (s statusError) Error() string {
	if s == http.StatusForbidden || s == http.StatusTooManyRequests {
		return fmt.Sprintf("GitHub rate limit (HTTP %d)", int(s))
	}
	return fmt.Sprintf("HTTP %d", int(s))
}

func (c Client) get(ctx context.Context, path string, limit int64, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.BaseURL, "/")+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return statusError(resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, limit)).Decode(into)
}

// Latest is the newest release. A 404 is ErrNoRelease.
func (c Client) Latest(ctx context.Context) (Release, error) {
	var r struct {
		TagName    string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := c.get(ctx, "/repos/"+Repo+"/releases/latest", releaseCap, &r); err != nil {
		var s statusError
		if errors.As(err, &s) && s == http.StatusNotFound {
			return Release{}, ErrNoRelease
		}
		if errors.As(err, &s) {
			return Release{}, err
		}
		return Release{}, fmt.Errorf("read the latest release: %w", err)
	}
	v, ok := ParseRelease(r.TagName)
	if !ok || r.Draft || r.Prerelease {
		return Release{}, fmt.Errorf("the latest release's tag %q isn't a version", clean(r.TagName))
	}
	return Release{Version: v, Tag: r.TagName, URL: ReleaseURL(r.TagName)}, nil
}

// Affecting lists the published, not withdrawn advisories whose range holds
// current. An advisory without a readable range counts (fail toward
// warning). A 404 is no advisories.
func (c Client) Affecting(ctx context.Context, current Version) ([]Advisory, error) {
	var raw []struct {
		GHSAID          string  `json:"ghsa_id"`
		Summary         string  `json:"summary"`
		WithdrawnAt     *string `json:"withdrawn_at"`
		Vulnerabilities []struct {
			Range   string `json:"vulnerable_version_range"`
			Patched string `json:"patched_versions"`
		} `json:"vulnerabilities"`
	}
	if err := c.get(ctx, "/repos/"+Repo+"/security-advisories?state=published&per_page=100", advisoriesCap, &raw); err != nil {
		var s statusError
		if errors.As(err, &s) && s == http.StatusNotFound {
			return []Advisory{}, nil
		}
		return nil, fmt.Errorf("read the security advisories: %w", err)
	}
	out := []Advisory{}
	for _, a := range raw {
		if a.WithdrawnAt != nil {
			continue
		}
		adv := Advisory{ID: clean(a.GHSAID), Summary: clean(a.Summary)}
		hit := len(a.Vulnerabilities) == 0
		for _, vu := range a.Vulnerabilities {
			if in, ok := Affected(vu.Range, current); in || !ok {
				adv.Range, adv.Patched, hit = clean(vu.Range), clean(vu.Patched), true
				break
			}
		}
		if hit {
			out = append(out, adv)
		}
	}
	return out, nil
}

// Result is one look: when, what it found, and why it failed.
type Result struct {
	At         time.Time
	Latest     *Release
	Advisories []Advisory
	Err        error
}

// Look asks for the latest release and, only when it is newer than current,
// for the advisories that affect current; both within budget. A failure is
// in Result.Err and is never shown to a person except by doctor.
func Look(ctx context.Context, c Client, current Version, now func() time.Time, budget time.Duration) Result {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	r := Result{At: now().UTC().Truncate(time.Second)} // like every other contract time
	rel, err := c.Latest(ctx)
	if err != nil {
		r.Err = err
		return r
	}
	r.Latest = &rel
	if Compare(rel.Version, current) <= 0 {
		return r
	}
	adv, err := c.Affecting(ctx, current)
	if err != nil {
		r.Err = err // the release stands; the ordinary reminder still works
		return r
	}
	r.Advisories = adv
	return r
}

// clean drops control characters (C0, DEL, C1), format characters (bidi
// overrides and isolates, zero-width marks) and the line and paragraph
// separators, and cuts to 200 runes, so text from the network can't move a
// terminal's cursor, reorder a line, break it or flood it.
func clean(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == 0x2028 || r == 0x2029 || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
	if rs := []rune(s); len(rs) > 200 {
		s = string(rs[:200])
	}
	return s
}
