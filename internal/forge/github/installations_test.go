package github_test

import (
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	ghforge "github.com/ghostbranch-labs/ghostbranch/internal/forge/github"
)

var _ forge.Installer = (*ghforge.Client)(nil)

func TestUserInstallationsPaginates(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /user/installations", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			writeJSON(w, map[string]any{"total_count": 3, "installations": []any{
				map[string]any{"id": 9, "repository_selection": "all", "account": map[string]any{"login": "acme", "type": "Organization"}},
			}})
			return
		}
		w.Header().Set("Link", `<http://`+r.Host+`/user/installations?page=2>; rel="next"`)
		writeJSON(w, map[string]any{"total_count": 3, "installations": []any{
			map[string]any{"id": 7, "repository_selection": "selected", "account": map[string]any{"login": "ayushgux", "type": "User"}},
			map[string]any{"id": 8, "repository_selection": "all", "account": map[string]any{"login": "other", "type": "User"}},
		}})
	})
	c := newClient(t, mux)
	got, err := c.UserInstallations(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []forge.Installation{
		{ID: 7, Account: "ayushgux", AccountType: "User", Selection: "selected"},
		{ID: 8, Account: "other", AccountType: "User", Selection: "all"},
		{ID: 9, Account: "acme", AccountType: "Organization", Selection: "all"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestUserInstallationsForbiddenForNonAppToken(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusUnauthorized} {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /user/installations", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"nope"}`, status)
		})
		if _, err := newClient(t, mux).UserInstallations(t.Context()); !errors.Is(err, forge.ErrForbidden) {
			t.Fatalf("status %d: err = %v", status, err)
		}
	}
}

func TestInstallationReposPaginates(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /user/installations/7/repositories", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			writeJSON(w, map[string]any{"total_count": 2, "repositories": []any{map[string]any{"full_name": "o/b"}}})
			return
		}
		w.Header().Set("Link", `<http://`+r.Host+`/user/installations/7/repositories?page=2>; rel="next"`)
		writeJSON(w, map[string]any{"total_count": 2, "repositories": []any{map[string]any{"full_name": "o/a"}}})
	})
	c := newClient(t, mux)
	got, err := c.InstallationRepos(t.Context(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"o/a", "o/b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestInstallationReposForbidden(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /user/installations/7/repositories", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"bad"}`, http.StatusUnauthorized)
	})
	if _, err := newClient(t, mux).InstallationRepos(t.Context(), 7); !errors.Is(err, forge.ErrForbidden) {
		t.Fatalf("err = %v", err)
	}
}

func TestConfigureURL(t *testing.T) {
	u := forge.Installation{ID: 7, Account: "ayushgux"}
	if got, want := u.ConfigureURL(), "https://github.com/settings/installations/7"; got != want {
		t.Fatalf("user: %s", got)
	}
	o := forge.Installation{ID: 9, Account: "acme", AccountType: "Organization"}
	if got, want := o.ConfigureURL(), "https://github.com/organizations/acme/settings/installations/9"; got != want {
		t.Fatalf("org: %s", got)
	}
}
