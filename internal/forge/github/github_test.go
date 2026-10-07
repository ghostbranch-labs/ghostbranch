package github_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	ghforge "github.com/ghostbranch-labs/ghostbranch/internal/forge/github"
)

var _ forge.Forge = (*ghforge.Client)(nil)

func newClient(t *testing.T, mux *http.ServeMux) *ghforge.Client {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c, err := ghforge.NewWithBaseURL("tok", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func TestGetRepoAndErrors(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/acme/widget", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("missing auth header: %q", r.Header.Get("Authorization"))
		}
		writeJSON(w, map[string]any{"full_name": "acme/widget", "private": false, "fork": false,
			"default_branch": "main", "clone_url": "https://github.com/acme/widget.git"})
	})
	mux.HandleFunc("GET /repos/acme/none", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})
	mux.HandleFunc("GET /repos/acme/flaky", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"bad gateway"}`, http.StatusBadGateway)
	})
	c := newClient(t, mux)
	ctx := context.Background()
	r, err := c.GetRepo(ctx, "acme/widget")
	if err != nil || r.FullName != "acme/widget" || r.Private || r.CloneURL != "https://github.com/acme/widget.git" {
		t.Fatalf("GetRepo = %+v %v", r, err)
	}
	if _, err := c.GetRepo(ctx, "acme/none"); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("404: %v", err)
	}
	if _, err := c.GetRepo(ctx, "acme/flaky"); !errors.Is(err, forge.ErrTransient) {
		t.Fatalf("502: %v", err)
	}
}

func TestCreatePrivateRepoInOrgAndUser(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"login": "octo", "id": 42})
	})
	var orgBody, userBody map[string]any
	mux.HandleFunc("POST /orgs/acme/repos", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&orgBody)
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, map[string]any{"full_name": "acme/widget-shadow", "private": true, "clone_url": "u"})
	})
	mux.HandleFunc("POST /user/repos", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&userBody)
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, map[string]any{"full_name": "octo/x-shadow", "private": true, "clone_url": "u"})
	})
	c := newClient(t, mux)
	ctx := context.Background()
	if r, err := c.CreatePrivateRepo(ctx, "acme/widget-shadow"); err != nil || !r.Private {
		t.Fatalf("org create = %+v %v", r, err)
	}
	if orgBody["private"] != true || orgBody["name"] != "widget-shadow" {
		t.Fatalf("org body = %v", orgBody)
	}
	if _, err := c.CreatePrivateRepo(ctx, "octo/x-shadow"); err != nil {
		t.Fatal(err)
	}
	if userBody["private"] != true {
		t.Fatalf("user body = %v", userBody)
	}
}

func TestPRsChecksAndDefaultBranch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("PATCH /repos/acme/s", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), `"default_branch":"ghostbranch"`) {
			t.Errorf("PATCH body = %s", b)
		}
		writeJSON(w, map[string]any{"full_name": "acme/s"})
	})
	mux.HandleFunc("GET /repos/acme/s/pulls", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("head") != "acme:ghostbranch-gate/x" || q.Get("base") != "main" || q.Get("state") != "open" {
			t.Errorf("pulls query = %v", q)
		}
		writeJSON(w, []map[string]any{{"number": 3, "state": "open", "draft": true, "html_url": "h",
			"head": map[string]any{"ref": "ghostbranch-gate/x"}, "base": map[string]any{"ref": "main"}}})
	})
	mux.HandleFunc("POST /repos/acme/s/pulls", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["draft"] != true || body["head"] != "b" {
			t.Errorf("create body = %v", body)
		}
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, map[string]any{"number": 4, "state": "open", "draft": true,
			"head": map[string]any{"ref": "b"}, "base": map[string]any{"ref": "main"}})
	})
	mux.HandleFunc("GET /repos/acme/s/commits/abc/check-runs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"total_count": 1, "check_runs": []map[string]any{
			{"name": "build", "status": "completed", "conclusion": "success"}}})
	})
	var srvURL string
	mux.HandleFunc("GET /repos/acme/s/commits/abc/status", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			writeJSON(w, map[string]any{"state": "pending", "statuses": []map[string]any{
				{"context": "lint", "state": "error"}}})
			return
		}
		w.Header().Set("Link", `<`+srvURL+`/repos/acme/s/commits/abc/status?page=2&per_page=100>; rel="next"`)
		writeJSON(w, map[string]any{"state": "pending", "statuses": []map[string]any{
			{"context": "legacy", "state": "pending"}}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	srvURL = srv.URL
	c, err := ghforge.NewWithBaseURL("tok", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.SetDefaultBranch(ctx, "acme/s", "ghostbranch"); err != nil {
		t.Fatal(err)
	}
	pr, err := c.FindOpenPR(ctx, "acme/s", "ghostbranch-gate/x", "main")
	if err != nil || pr == nil || pr.Number != 3 || !pr.Draft || pr.Head != "ghostbranch-gate/x" {
		t.Fatalf("FindOpenPR = %+v %v", pr, err)
	}
	if p, err := c.CreatePR(ctx, "acme/s", forge.NewPR{Title: "t", Head: "b", Base: "main", Draft: true}); err != nil || p.Number != 4 {
		t.Fatalf("CreatePR = %+v %v", p, err)
	}
	checks, err := c.ListChecks(ctx, "acme/s", "abc")
	if err != nil || len(checks) != 3 {
		t.Fatalf("ListChecks = %+v %v", checks, err)
	}
	want := map[string]forge.Check{
		"build":  {Name: "build", Status: "completed", Conclusion: "success"},
		"legacy": {Name: "legacy", Status: "in_progress"},
		"lint":   {Name: "lint", Status: "completed", Conclusion: "failure"},
	}
	for _, ch := range checks {
		if ch != want[ch.Name] {
			t.Errorf("check %s = %+v, want %+v", ch.Name, ch, want[ch.Name])
		}
	}
}

const patDenied = `{"message":"Resource not accessible by personal access token"}`

func denyCheckRuns(mux *http.ServeMux) {
	mux.HandleFunc("GET /repos/acme/s/commits/abc/check-runs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(patDenied))
	})
}

func noStatuses(mux *http.ServeMux) {
	mux.HandleFunc("GET /repos/acme/s/commits/abc/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"state": "success", "statuses": []map[string]any{
			{"context": "legacy", "state": "success"}}})
	})
}

func TestListChecksFallsBackToWorkflowRunsOn403(t *testing.T) {
	mux := http.NewServeMux()
	denyCheckRuns(mux)
	noStatuses(mux)
	mux.HandleFunc("GET /repos/acme/s/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("head_sha") != "abc" {
			t.Errorf("runs query = %v", r.URL.Query())
		}
		writeJSON(w, map[string]any{"total_count": 3, "workflow_runs": []map[string]any{
			{"id": 1, "name": "CI", "status": "completed", "conclusion": "success"},
			{"id": 2, "name": "Lint", "status": "in_progress"},
			{"id": 3, "name": "", "status": "queued"}}})
	})
	c := newClient(t, mux)
	if c.ChecksLimited("acme/s") {
		t.Fatal("limited before any call")
	}
	checks, err := c.ListChecks(context.Background(), "acme/s", "abc")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]forge.Check{
		"CI":         {Name: "CI", Status: "completed", Conclusion: "success"},
		"Lint":       {Name: "Lint", Status: "in_progress"},
		"workflow 3": {Name: "workflow 3", Status: "in_progress"},
		"legacy":     {Name: "legacy", Status: "completed", Conclusion: "success"},
	}
	if len(checks) != len(want) {
		t.Fatalf("checks = %+v", checks)
	}
	for _, ch := range checks {
		if ch != want[ch.Name] {
			t.Errorf("check %s = %+v, want %+v", ch.Name, ch, want[ch.Name])
		}
	}
	if !c.ChecksLimited("acme/s") {
		t.Error("ChecksLimited(acme/s) = false after fallback")
	}
	if c.ChecksLimited("acme/other") {
		t.Error("ChecksLimited(acme/other) = true")
	}
}

func TestListChecksWorkflowRunsPaging(t *testing.T) {
	mux := http.NewServeMux()
	denyCheckRuns(mux)
	noStatuses(mux)
	var srvURL string
	mux.HandleFunc("GET /repos/acme/s/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			writeJSON(w, map[string]any{"workflow_runs": []map[string]any{
				{"id": 2, "name": "B", "status": "completed", "conclusion": "failure"}}})
			return
		}
		w.Header().Set("Link", `<`+srvURL+`/repos/acme/s/actions/runs?head_sha=abc&page=2&per_page=100>; rel="next"`)
		writeJSON(w, map[string]any{"workflow_runs": []map[string]any{
			{"id": 1, "name": "A", "status": "completed", "conclusion": "success"}}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	srvURL = srv.URL
	c, err := ghforge.NewWithBaseURL("tok", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	checks, err := c.ListChecks(context.Background(), "acme/s", "abc")
	if err != nil || len(checks) != 3 {
		t.Fatalf("checks = %+v %v", checks, err)
	}
	if checks[0].Name != "A" || checks[1].Name != "B" || checks[1].Conclusion != "failure" {
		t.Errorf("checks = %+v", checks)
	}
}

func TestListChecksBothDeniedNamesTheFix(t *testing.T) {
	mux := http.NewServeMux()
	denyCheckRuns(mux)
	noStatuses(mux)
	mux.HandleFunc("GET /repos/acme/s/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(patDenied))
	})
	_, err := newClient(t, mux).ListChecks(context.Background(), "acme/s", "abc")
	if err == nil || !strings.Contains(err.Error(), "Actions: read") || !strings.Contains(err.Error(), "acme/s") {
		t.Fatalf("err = %v", err)
	}
}

func TestListChecksCheckRuns500DoesNotFallBack(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/acme/s/commits/abc/check-runs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	mux.HandleFunc("GET /repos/acme/s/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		t.Error("fell back on a 500")
	})
	c := newClient(t, mux)
	_, err := c.ListChecks(context.Background(), "acme/s", "abc")
	if !errors.Is(err, forge.ErrTransient) {
		t.Fatalf("err = %v, want transient", err)
	}
	if c.ChecksLimited("acme/s") {
		t.Error("limited after a 500")
	}
}

func TestPushAccessProbesReceivePack(t *testing.T) {
	var gotAuth, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotQuery = r.Header.Get("Authorization"), r.URL.RawQuery
		switch r.URL.Path {
		case "/acme/widget.git/info/refs":
			w.WriteHeader(http.StatusForbidden)
		case "/acme/widget-shadow.git/info/refs":
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := ghforge.New("tok").WithGitBase(srv.URL)
	ctx := context.Background()
	if a := c.PushAccess(ctx, "acme/widget"); a.Can != "false" || a.HTTPStatus != 403 {
		t.Fatalf("public: %+v", a)
	}
	if gotQuery != "service=git-receive-pack" || gotAuth != "Basic "+base64.StdEncoding.EncodeToString([]byte("x-access-token:tok")) {
		t.Fatalf("request: query %q auth %q", gotQuery, gotAuth)
	}
	if a := c.PushAccess(ctx, "acme/widget-shadow"); a.Can != "true" || a.HTTPStatus != 200 {
		t.Fatalf("shadow: %+v", a)
	}
	if a := c.PushAccess(ctx, "acme/missing"); a.Can != "unknown" || a.HTTPStatus != 404 {
		t.Fatalf("missing: %+v", a)
	}
	if a := c.PushAccess(ctx, "not-a-repo"); a.Can != "unknown" {
		t.Fatalf("bad name: %+v", a)
	}
	srv.Close()
	if a := c.PushAccess(ctx, "acme/widget"); a.Can != "unknown" || a.HTTPStatus != 0 {
		t.Fatalf("network error: %+v", a)
	}
}

func TestSecretNames(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/acme/widget-shadow/actions/secrets", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			writeJSON(w, map[string]any{"total_count": 3, "secrets": []map[string]any{{"name": "B_KEY"}}})
			return
		}
		w.Header().Set("Link", `<`+"http://"+r.Host+r.URL.Path+`?page=2>; rel="next"`)
		writeJSON(w, map[string]any{"total_count": 3, "secrets": []map[string]any{{"name": "A_KEY"}, {"name": "SHARED"}}})
	})
	mux.HandleFunc("GET /repos/acme/widget-shadow/actions/organization-secrets", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"total_count": 2, "secrets": []map[string]any{{"name": "SHARED"}, {"name": "ORG_KEY"}}})
	})
	mux.HandleFunc("GET /repos/acme/locked/actions/secrets", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Resource not accessible by personal access token"}`, http.StatusForbidden)
	})
	mux.HandleFunc("GET /repos/octo/mine/actions/secrets", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"total_count": 1, "secrets": []map[string]any{{"name": "MINE"}}})
	})
	mux.HandleFunc("GET /repos/octo/mine/actions/organization-secrets", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})
	c := newClient(t, mux)
	ctx := context.Background()
	got, err := c.SecretNames(ctx, "acme/widget-shadow")
	if err != nil || strings.Join(got, ",") != "A_KEY,B_KEY,ORG_KEY,SHARED" {
		t.Fatalf("SecretNames = %v %v", got, err)
	}
	if _, err := c.SecretNames(ctx, "acme/locked"); !errors.Is(err, forge.ErrForbidden) {
		t.Fatalf("403: %v", err)
	}
	if got, err := c.SecretNames(ctx, "octo/mine"); err != nil || strings.Join(got, ",") != "MINE" {
		t.Fatalf("user repo without org secrets: %v %v", got, err)
	}
}

func TestCreatePrivateRepoForbidden(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"login": "octo", "id": 1})
	})
	mux.HandleFunc("POST /orgs/acme/repos", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Resource not accessible by personal access token"}`, http.StatusForbidden)
	})
	mux.HandleFunc("POST /orgs/hidden/repos", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})
	c := newClient(t, mux)
	for _, full := range []string{"acme/widget-shadow", "hidden/widget-shadow"} {
		if _, err := c.CreatePrivateRepo(context.Background(), full); !errors.Is(err, forge.ErrForbidden) {
			t.Fatalf("%s: %v", full, err)
		}
	}
}
