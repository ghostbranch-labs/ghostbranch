// Package authtest is a fake GitHub OAuth server for device-flow and
// refresh tests. It listens on loopback only and issues obviously fake
// tokens (ghu_test_access_N, ghr_test_refresh_N). Like GitHub, it answers
// OAuth errors with HTTP 200 and an "error" field, and it rotates the
// refresh token on every refresh, so a second refresh with the old one
// fails the way it does at GitHub.
package authtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
)

const (
	ClientID    = "Iv1.test-client"
	DeviceCode  = "dc_test_1"
	UserCode    = "WDJB-MJHT"
	deviceGrant = "urn:ietf:params:oauth:grant-type:device_code"
)

// Options script the server. They are fixed when it starts, so tests never
// race the handler.
type Options struct {
	Pending       int    // polls answered authorization_pending before the approval
	SlowDown      bool   // the first poll answers slow_down (with interval 10)
	Deny          bool   // polls answer access_denied
	ExpiredCode   bool   // polls answer expired_token
	CodeExpiresIn int    // the device code's lifetime in seconds; 0 means 900
	RefreshToken  string // a valid refresh token to start with, for refresh-only tests
	RefreshBad    bool   // every refresh answers bad_refresh_token
	EchoSecrets   bool   // error replies repeat the posted form (tokens included) in error_description
	Fail500       bool   // every request fails with HTTP 500, its body repeating the posted form
	RefreshGate   func() // runs inside each refresh before it answers (the race test blocks here)
}

type Server struct {
	URL      string
	ClientID string
	srv      *httptest.Server
	o        Options

	mu        sync.Mutex
	polls     int
	refreshes int
	issued    int
	refresh   string // the one valid refresh token
	forms     []url.Values
}

// New starts a server and stops it when the test ends.
func New(t testing.TB, o Options) *Server {
	s := &Server{ClientID: ClientID, o: o, refresh: o.RefreshToken}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	s.URL = s.srv.URL
	t.Cleanup(s.srv.Close)
	return s
}

func (s *Server) Client() *http.Client { return s.srv.Client() }

// AccessToken and RefreshToken are the n-th pair the server issues (from 1).
func (s *Server) AccessToken(n int) string  { return fmt.Sprintf("ghu_test_access_%d", n) }
func (s *Server) RefreshToken(n int) string { return fmt.Sprintf("ghr_test_refresh_%d", n) }

func (s *Server) Polls() int     { s.mu.Lock(); defer s.mu.Unlock(); return s.polls }
func (s *Server) Refreshes() int { s.mu.Lock(); defer s.mu.Unlock(); return s.refreshes }

// Forms are the posted forms, in order.
func (s *Server) Forms() []url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]url.Values(nil), s.forms...)
}

func reply(w http.ResponseWriter, v map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) oauthError(w http.ResponseWriter, code string, form url.Values) {
	desc := "test error " + code
	if s.o.EchoSecrets {
		desc += ": " + form.Encode()
	}
	reply(w, map[string]any{"error": code, "error_description": desc, "error_uri": "https://docs.github.com/"})
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.Header.Get("Accept") != "application/json" {
		http.Error(w, "want POST with Accept: application/json", http.StatusNotAcceptable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	form := r.PostForm
	s.mu.Lock()
	s.forms = append(s.forms, form)
	s.mu.Unlock()
	if s.o.Fail500 {
		http.Error(w, "boom: "+form.Encode(), http.StatusInternalServerError)
		return
	}
	if form.Get("client_id") != s.ClientID {
		s.oauthError(w, "incorrect_client_credentials", form)
		return
	}
	switch r.URL.Path {
	case "/login/device/code":
		exp := s.o.CodeExpiresIn
		if exp == 0 {
			exp = 900
		}
		reply(w, map[string]any{"device_code": DeviceCode, "user_code": UserCode,
			"verification_uri": s.URL + "/login/device", "expires_in": exp, "interval": 5})
	case "/login/oauth/access_token":
		switch form.Get("grant_type") {
		case deviceGrant:
			s.poll(w, form)
		case "refresh_token":
			s.refreshGrant(w, form)
		default:
			s.oauthError(w, "unsupported_grant_type", form)
		}
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) poll(w http.ResponseWriter, form url.Values) {
	s.mu.Lock()
	s.polls++
	n := s.polls
	s.mu.Unlock()
	switch {
	case form.Get("device_code") != DeviceCode:
		s.oauthError(w, "incorrect_device_code", form)
		return
	case s.o.ExpiredCode:
		s.oauthError(w, "expired_token", form)
		return
	case s.o.Deny:
		s.oauthError(w, "access_denied", form)
		return
	}
	if s.o.SlowDown {
		if n == 1 {
			reply(w, map[string]any{"error": "slow_down", "interval": 10})
			return
		}
		n--
	}
	if n <= s.o.Pending {
		s.oauthError(w, "authorization_pending", form)
		return
	}
	s.issue(w)
}

func (s *Server) refreshGrant(w http.ResponseWriter, form url.Values) {
	s.mu.Lock()
	s.refreshes++
	s.mu.Unlock()
	if s.o.RefreshGate != nil {
		s.o.RefreshGate()
	}
	s.mu.Lock()
	ok := !s.o.RefreshBad && s.refresh != "" && form.Get("refresh_token") == s.refresh
	s.mu.Unlock()
	if !ok {
		s.oauthError(w, "bad_refresh_token", form)
		return
	}
	s.issue(w)
}

func (s *Server) issue(w http.ResponseWriter) {
	s.mu.Lock()
	s.issued++
	n := s.issued
	s.refresh = s.RefreshToken(n)
	s.mu.Unlock()
	reply(w, map[string]any{"access_token": s.AccessToken(n), "expires_in": 28800,
		"refresh_token": s.RefreshToken(n), "refresh_token_expires_in": 15897600,
		"token_type": "bearer", "scope": ""})
}
