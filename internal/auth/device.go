package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
)

const deviceGrant = "urn:ietf:params:oauth:grant-type:device_code"

// DeviceFlow is GitHub's OAuth device flow for the Ghostbranch GitHub App:
// POST /login/device/code, then poll POST /login/oauth/access_token. Its
// refresh needs no client secret (device-flow tokens), so there is no server.
type DeviceFlow struct {
	ClientID string
	BaseURL  string // "https://github.com"; tests point it at authtest
	HTTP     *http.Client
	Sleep    func(time.Duration)
	Now      func() time.Time
}

type DeviceCode struct {
	DeviceCode, UserCode, VerificationURI string
	ExpiresIn, Interval                   int
}

// String keeps the device code, a live secret until it expires, out of any
// %v, %+v, %#v or %s. The user code and URL are shown to the person anyway.
func (d DeviceCode) String() string {
	return fmt.Sprintf("auth.DeviceCode{UserCode:%q VerificationURI:%q ExpiresIn:%d Interval:%d DeviceCode:<redacted>}",
		d.UserCode, d.VerificationURI, d.ExpiresIn, d.Interval)
}

func (d DeviceCode) GoString() string { return d.String() }

// oauthReply is every field GitHub's device and token endpoints answer
// with. error_description is deliberately not read: it is free text from
// the server and is never shown.
type oauthReply struct {
	DeviceCode            string `json:"device_code"`
	UserCode              string `json:"user_code"`
	VerificationURI       string `json:"verification_uri"`
	ExpiresIn             int    `json:"expires_in"`
	Interval              int    `json:"interval"`
	AccessToken           string `json:"access_token"`
	RefreshToken          string `json:"refresh_token"`
	RefreshTokenExpiresIn int    `json:"refresh_token_expires_in"`
	Error                 string `json:"error"`
}

var oauthCode = regexp.MustCompile(`^[a-z_]{1,64}$`)

func (d DeviceFlow) base() string {
	if d.BaseURL == "" {
		return "https://github.com"
	}
	return strings.TrimRight(d.BaseURL, "/")
}

func (d DeviceFlow) client() *http.Client {
	if d.HTTP != nil {
		return d.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (d DeviceFlow) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d DeviceFlow) sleep(t time.Duration) {
	if d.Sleep != nil {
		d.Sleep(t)
		return
	}
	time.Sleep(t)
}

// post sends one OAuth form and decodes the JSON reply. Secrets travel only
// in the form body, never in a URL, and no error quotes the reply.
func (d DeviceFlow) post(ctx context.Context, path string, form url.Values) (oauthReply, error) {
	var r oauthReply
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.base()+path, strings.NewReader(form.Encode()))
	if err != nil {
		return r, contract.Wrap(contract.Internal, err, "build the GitHub login request")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := d.client().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return r, ctx.Err()
		}
		e := contract.New(contract.ForgeError, "GitHub's login service can't be reached; check the network and try again")
		e.Retryable, e.Err = true, err
		return r, e
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		e := contract.New(contract.ForgeError, "GitHub's login service answered HTTP %d", resp.StatusCode)
		e.Retryable = resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests
		return r, e
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&r) != nil {
		return r, contract.New(contract.ForgeError, "GitHub's login service sent a reply that isn't JSON")
	}
	if r.Error != "" && !oauthCode.MatchString(r.Error) {
		r.Error = "unknown_error"
	}
	return r, nil
}

// refused turns an OAuth error code (already checked to be [a-z_]) into
// LOGIN_UNAVAILABLE.
func refused(code string) error {
	switch code {
	case "access_denied":
		return unavailable("denied", "the login was declined on GitHub; nothing was stored")
	case "expired_token":
		return unavailable("expired_code", "the login code expired before it was approved; run `ghostbranch login` again")
	case "device_flow_disabled":
		return unavailable("refused", "the Ghostbranch GitHub App has device flow turned off; use `ghostbranch login --with-token`")
	}
	return unavailable("refused", "GitHub refused the login (%s)", code)
}

func noClientID() error {
	return unavailable("no_client_id", "this build has no GitHub App client ID; pipe a token to `ghostbranch login --with-token`, or set GHOSTBRANCH_TOKEN")
}

// Start asks GitHub for a device code and the code the person types.
func (d DeviceFlow) Start(ctx context.Context) (DeviceCode, error) {
	if d.ClientID == "" {
		return DeviceCode{}, noClientID()
	}
	r, err := d.post(ctx, "/login/device/code", url.Values{"client_id": {d.ClientID}})
	if err != nil {
		return DeviceCode{}, err
	}
	if r.Error != "" {
		return DeviceCode{}, refused(r.Error)
	}
	if r.DeviceCode == "" || r.UserCode == "" || r.VerificationURI == "" {
		return DeviceCode{}, contract.New(contract.ForgeError, "GitHub's login service sent an incomplete device code")
	}
	return DeviceCode{DeviceCode: r.DeviceCode, UserCode: r.UserCode, VerificationURI: r.VerificationURI,
		ExpiresIn: r.ExpiresIn, Interval: r.Interval}, nil
}

// Poll waits for the person to approve the code. It sleeps the interval
// before each poll (at least 5 s), adds 5 s on slow_down (or takes GitHub's
// larger interval), stops when the code's life is over, and returns ctx's
// error on cancel. A network blip while waiting is retried. Account is left
// empty: the caller fills it from the API.
func (d DeviceFlow) Poll(ctx context.Context, dc DeviceCode) (Login, error) {
	interval := time.Duration(max(dc.Interval, 5)) * time.Second
	deadline := d.now().Add(time.Duration(dc.ExpiresIn) * time.Second)
	form := url.Values{"client_id": {d.ClientID}, "device_code": {dc.DeviceCode}, "grant_type": {deviceGrant}}
	for {
		d.sleep(interval)
		if err := ctx.Err(); err != nil {
			return Login{}, err
		}
		if dc.ExpiresIn > 0 && !d.now().Before(deadline) {
			return Login{}, refused("expired_token")
		}
		r, err := d.post(ctx, "/login/oauth/access_token", form)
		if err != nil {
			if ce := contract.From(err); ce.Retryable && ctx.Err() == nil {
				continue
			}
			return Login{}, err
		}
		switch r.Error {
		case "":
			return d.login(r)
		case "authorization_pending":
		case "slow_down":
			interval += 5 * time.Second
			if s := time.Duration(r.Interval) * time.Second; s > interval {
				interval = s
			}
		default:
			return Login{}, refused(r.Error)
		}
	}
}

// Refresh trades a refresh token for a new pair. GitHub invalidates the old
// pair, so callers refresh under Source.Lock. A refused refresh token is
// LOGIN_EXPIRED.
func (d DeviceFlow) Refresh(ctx context.Context, refreshToken string) (Login, error) {
	if d.ClientID == "" {
		return Login{}, noClientID()
	}
	r, err := d.post(ctx, "/login/oauth/access_token", url.Values{"client_id": {d.ClientID},
		"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}})
	if err != nil {
		return Login{}, err
	}
	switch r.Error {
	case "":
		return d.login(r)
	case "bad_refresh_token":
		return Login{}, expired()
	}
	return Login{}, unavailable("refresh_failed", "GitHub refused to refresh the stored login (%s); run `ghostbranch login`", r.Error)
}

func (d DeviceFlow) login(r oauthReply) (Login, error) {
	if r.AccessToken == "" {
		return Login{}, contract.New(contract.ForgeError, "GitHub's login service sent no token")
	}
	now := d.now()
	l := Login{Token: r.AccessToken, RefreshToken: r.RefreshToken, Kind: "app", SchemaVersion: contract.SchemaVersion}
	if r.ExpiresIn > 0 {
		l.Expires = now.Add(time.Duration(r.ExpiresIn) * time.Second)
	}
	if r.RefreshTokenExpiresIn > 0 {
		l.RefreshExpires = now.Add(time.Duration(r.RefreshTokenExpiresIn) * time.Second)
	}
	return l, nil
}
