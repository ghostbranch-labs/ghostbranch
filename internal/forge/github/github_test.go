package github_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	gh "github.com/google/go-github/v92/github"

	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	ghforge "github.com/ghostbranch-labs/ghostbranch/internal/forge/github"
)

var (
	_ forge.Forge            = (*ghforge.Client)(nil)
	_ forge.PermissionProber = (*ghforge.Client)(nil)
)

func newClient(t *testing.T, mux *http.ServeMux) *ghforge.Client {
	t.Helper()
	return newTokenClient(t, mux, "tok")
}

func newTokenClient(t *testing.T, mux *http.ServeMux, token string) *ghforge.Client {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c, err := ghforge.NewWithBaseURL(token, srv.URL)
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
	if err == nil || !strings.Contains(err.Error(), "Actions: read") || !strings.Contains(err.Error(), "acme/s") || !errors.Is(err, forge.ErrForbidden) {
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
	c, err := ghforge.New("tok")
	if err != nil {
		t.Fatal(err)
	}
	c.WithGitBase(srv.URL)
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

func TestPushRunsListsPushRunsForTheSHA(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/acme/s/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("head_sha") != "abc" || q.Get("event") != "push" {
			t.Errorf("runs query = %v", q)
		}
		if q.Get("page") == "2" {
			writeJSON(w, map[string]any{"workflow_runs": []map[string]any{
				{"id": 2, "name": "Lint", "status": "completed", "conclusion": "cancelled", "html_url": "https://github.com/acme/s/actions/runs/2"}}})
			return
		}
		w.Header().Set("Link", `<`+"http://"+r.Host+r.URL.Path+`?head_sha=abc&event=push&page=2>; rel="next"`)
		writeJSON(w, map[string]any{"workflow_runs": []map[string]any{
			{"id": 1, "name": "CI", "status": "queued", "html_url": "https://github.com/acme/s/actions/runs/1"}}})
	})
	mux.HandleFunc("GET /repos/acme/locked/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Resource not accessible by personal access token"}`, http.StatusForbidden)
	})
	c := newClient(t, mux)
	ctx := context.Background()
	runs, err := c.PushRuns(ctx, "acme/s", "abc")
	if err != nil {
		t.Fatal(err)
	}
	want := []forge.Run{
		{ID: 1, Workflow: "CI", Status: "queued", URL: "https://github.com/acme/s/actions/runs/1"},
		{ID: 2, Workflow: "Lint", Status: "completed", Conclusion: "cancelled", URL: "https://github.com/acme/s/actions/runs/2"},
	}
	if len(runs) != len(want) || runs[0] != want[0] || runs[1] != want[1] {
		t.Fatalf("runs = %+v", runs)
	}
	if _, err := c.PushRuns(ctx, "acme/locked", "abc"); !errors.Is(err, forge.ErrForbidden) {
		t.Fatalf("403: %v", err)
	}
}

func TestCancelRun(t *testing.T) {
	mux := http.NewServeMux()
	var cancelled []string
	mux.HandleFunc("POST /repos/acme/s/actions/runs/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		cancelled = append(cancelled, id)
		switch id {
		case "1":
			w.WriteHeader(http.StatusAccepted)
			writeJSON(w, map[string]any{})
		case "2":
			http.Error(w, `{"message":"Cannot cancel a workflow run that is completed."}`, http.StatusConflict)
		case "3":
			http.Error(w, `{"message":"Resource not accessible by personal access token"}`, http.StatusForbidden)
		default:
			http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
		}
	})
	c := newClient(t, mux)
	ctx := context.Background()
	if err := c.CancelRun(ctx, "acme/s", 1); err != nil {
		t.Fatalf("202: %v", err)
	}
	if err := c.CancelRun(ctx, "acme/s", 2); !errors.Is(err, forge.ErrConflict) {
		t.Fatalf("409: %v", err)
	}
	if err := c.CancelRun(ctx, "acme/s", 3); !errors.Is(err, forge.ErrForbidden) {
		t.Fatalf("403: %v", err)
	}
	if err := c.CancelRun(ctx, "acme/s", 4); err == nil || errors.Is(err, forge.ErrForbidden) {
		t.Fatalf("500: %v", err)
	}
	if strings.Join(cancelled, ",") != "1,2,3,4" {
		t.Fatalf("cancelled = %v", cancelled)
	}
}

func TestForceCancelRun(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	var forced []string
	mux.HandleFunc("POST /repos/o/r/actions/runs/{id}/force-cancel", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		forced = append(forced, id)
		switch id {
		case "7":
			w.WriteHeader(http.StatusAccepted)
			writeJSON(w, map[string]any{})
		case "8":
			http.Error(w, `{"message":"Cannot force-cancel a workflow run that is completed."}`, http.StatusConflict)
		case "9":
			http.Error(w, `{"message":"Resource not accessible by personal access token"}`, http.StatusForbidden)
		default:
			http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
		}
	})
	c := newClient(t, mux)
	ctx := context.Background()
	if err := c.ForceCancelRun(ctx, "o/r", 7); err != nil {
		t.Fatalf("202: %v", err)
	}
	if err := c.ForceCancelRun(ctx, "o/r", 8); !errors.Is(err, forge.ErrConflict) {
		t.Fatalf("409: %v", err)
	}
	if err := c.ForceCancelRun(ctx, "o/r", 9); !errors.Is(err, forge.ErrForbidden) {
		t.Fatalf("403: %v", err)
	}
	if err := c.ForceCancelRun(ctx, "o/r", 10); err == nil || errors.Is(err, forge.ErrForbidden) || errors.Is(err, forge.ErrConflict) {
		t.Fatalf("500: %v", err)
	}
	if strings.Join(forced, ",") != "7,8,9,10" {
		t.Fatalf("force-cancelled = %v", forced)
	}
}

func TestGetPRReportsMerged(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/acme/widget/pulls/7", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"number": 7, "state": "closed", "merged": true,
			"head": map[string]any{"ref": "feat"}, "base": map[string]any{"ref": "main"}})
	})
	c := newClient(t, mux)
	pr, err := c.GetPR(context.Background(), "acme/widget", 7)
	if err != nil || !pr.Merged || pr.State != "closed" {
		t.Fatalf("GetPR = %+v %v", pr, err)
	}
}

func TestClosePullRequest(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	var body map[string]any
	mux.HandleFunc("PATCH /repos/acme/widget/pulls/3", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		writeJSON(w, map[string]any{"number": 3, "state": "closed"})
	})
	mux.HandleFunc("PATCH /repos/acme/widget/pulls/9", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})
	mux.HandleFunc("PATCH /repos/acme/widget/pulls/4", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Resource not accessible by personal access token"}`, http.StatusForbidden)
	})
	c := newClient(t, mux)
	ctx := context.Background()
	if err := c.ClosePullRequest(ctx, "acme/widget", 3); err != nil {
		t.Fatal(err)
	}
	if len(body) != 1 || body["state"] != "closed" {
		t.Fatalf("request body = %v", body)
	}
	if err := c.ClosePullRequest(ctx, "acme/widget", 9); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("404: %v", err)
	}
	if err := c.ClosePullRequest(ctx, "acme/widget", 4); !errors.Is(err, forge.ErrForbidden) {
		t.Fatalf("403: %v", err)
	}
}

// Every call maps a plain 403 to forge.ErrForbidden; a rate-limit 403 is
// transient, never forbidden.
func TestA403IsForbiddenOnEveryCall(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	deny := func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Resource not accessible by personal access token"}`, http.StatusForbidden)
	}
	for _, p := range []string{"GET /repos/acme/s", "PATCH /repos/acme/s", "GET /repos/acme/s/pulls", "POST /repos/acme/s/pulls",
		"GET /repos/acme/s/pulls/1", "GET /user", "GET /repos/acme/s/commits/abc/status"} {
		mux.HandleFunc(p, deny)
	}
	// Check runs are readable, so ListChecks reaches the status listing.
	mux.HandleFunc("GET /repos/acme/s/commits/abc/check-runs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"total_count": 0, "check_runs": []any{}})
	})
	c := newClient(t, mux)
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"GetRepo":          func() error { _, err := c.GetRepo(ctx, "acme/s"); return err },
		"SetDefaultBranch": func() error { return c.SetDefaultBranch(ctx, "acme/s", "main") },
		"FindOpenPR":       func() error { _, err := c.FindOpenPR(ctx, "acme/s", "feat", "main"); return err },
		"CreatePR": func() error {
			_, err := c.CreatePR(ctx, "acme/s", forge.NewPR{Title: "t", Head: "feat", Base: "main"})
			return err
		},
		"GetPR":       func() error { _, err := c.GetPR(ctx, "acme/s", 1); return err },
		"CurrentUser": func() error { _, err := c.CurrentUser(ctx); return err },
		"ListChecks":  func() error { _, err := c.ListChecks(ctx, "acme/s", "abc"); return err },
	} {
		if err := call(); !errors.Is(err, forge.ErrForbidden) {
			t.Errorf("%s: %v, want ErrForbidden", name, err)
		}
	}
	limited := http.NewServeMux()
	limited.HandleFunc("GET /repos/acme/s", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "60")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", "4102444800")
		http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
	})
	if _, err := newClient(t, limited).GetRepo(ctx, "acme/s"); errors.Is(err, forge.ErrForbidden) || !errors.Is(err, forge.ErrTransient) {
		t.Fatalf("rate limit: %v, want transient and not forbidden", err)
	}
}

// An empty token never builds an unauthenticated client: it would read
// public repos and then fail on private ones with confusing 404s.
func TestNoClientWithoutAToken(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	for _, tok := range []string{"", "  ", "\n\t"} {
		if c, err := ghforge.New(tok); c != nil || !errors.Is(err, ghforge.ErrNoToken) {
			t.Errorf("New(%q) = %v, %v", tok, c, err)
		}
		if c, err := ghforge.NewWithBaseURL(tok, srv.URL); c != nil || !errors.Is(err, ghforge.ErrNoToken) {
			t.Errorf("NewWithBaseURL(%q) = %v, %v", tok, c, err)
		}
	}
	var zero ghforge.Client
	if a := zero.WithGitBase(srv.URL).PushAccess(context.Background(), "acme/widget"); a.Can != "unknown" || a.HTTPStatus != 0 {
		t.Errorf("a client without a token probed: %+v", a)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("%d unauthenticated requests", n)
	}
}

func TestTokenWhitespaceIsTrimmed(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer ghu_test" {
			t.Errorf("Authorization = %q", got)
		}
		writeJSON(w, map[string]any{"login": "octo", "id": 1})
	})
	if _, err := newTokenClient(t, mux, " ghu_test\r\n").CurrentUser(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// Only failures that may pass on their own are transient: a bad token, a
// validation error or the caller's own cancel are not. Every error keeps
// go-github's error in its chain, and none shows the token.
func TestErrorsKeepTheirChainAndRetryOnlyWhatMayPass(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	status := func(code int, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_, _ = w.Write([]byte(body))
		}
	}
	mux.HandleFunc("GET /repos/acme/badtoken", status(http.StatusUnauthorized, `{"message":"Bad credentials"}`))
	mux.HandleFunc("GET /repos/acme/busy", status(http.StatusTooManyRequests, `{"message":"Too Many Requests"}`))
	mux.HandleFunc("GET /repos/acme/down", status(http.StatusServiceUnavailable, `{"message":"unavailable"}`))
	mux.HandleFunc("GET /repos/acme/invalid", status(http.StatusUnprocessableEntity, `{"message":"Validation Failed"}`))
	c := newTokenClient(t, mux, "ghu_test")
	ctx := context.Background()
	for repo, transient := range map[string]bool{"acme/badtoken": false, "acme/busy": true, "acme/down": true, "acme/invalid": false} {
		_, err := c.GetRepo(ctx, repo)
		var ge *gh.ErrorResponse
		if err == nil || errors.Is(err, forge.ErrTransient) != transient || errors.Is(err, forge.ErrForbidden) || !errors.As(err, &ge) {
			t.Errorf("%s: %v (transient %v, want %v; chain kept %v)", repo, err, errors.Is(err, forge.ErrTransient), transient, errors.As(err, &ge))
		}
		if err != nil && strings.Contains(err.Error(), "ghu_test") {
			t.Errorf("%s: the token is in the error: %v", repo, err)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.GetRepo(cancelled, "acme/down"); !errors.Is(err, context.Canceled) || errors.Is(err, forge.ErrTransient) {
		t.Fatalf("cancelled: %v, want context.Canceled and not transient", err)
	}
}

// A 403 names the missing permission when GitHub's headers do: the first
// permission set for fine-grained and app tokens, the accepted scope for a
// classic token that holds none of them. Without them it stays a plain
// ErrForbidden; nothing is guessed.
func TestA403NamesTheMissingPermission(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	deny := func(perms, accepted, held string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if perms != "" {
				w.Header().Set("X-Accepted-GitHub-Permissions", perms)
			}
			if accepted != "" {
				w.Header().Set("X-Accepted-OAuth-Scopes", accepted)
				w.Header().Set("X-OAuth-Scopes", held)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(patDenied))
		}
	}
	mux.HandleFunc("POST /repos/acme/one/pulls", deny("pull_requests=write", "", ""))
	mux.HandleFunc("POST /repos/acme/two/pulls", deny("pull_requests=write,contents=read; issues=write,contents=read", "", ""))
	mux.HandleFunc("POST /repos/acme/classic/pulls", deny("", "repo", "read:org, gist"))
	mux.HandleFunc("POST /repos/acme/plain/pulls", deny("", "", ""))
	mux.HandleFunc("POST /repos/acme/held/pulls", deny("", "repo", "repo, read:org"))
	mux.HandleFunc("GET /repos/acme/two/actions/secrets", deny("secrets=read", "", ""))
	c := newTokenClient(t, mux, "ghu_test")
	ctx := context.Background()
	open := func(repo string) error {
		_, err := c.CreatePR(ctx, repo, forge.NewPR{Title: "t", Head: "feat", Base: "main"})
		return err
	}
	for repo, want := range map[string]string{"acme/one": "Pull requests: write",
		"acme/two": "Pull requests: write and Contents: read", "acme/classic": "OAuth scope: repo"} {
		err := open(repo)
		var pe *forge.PermissionError
		var ge *gh.ErrorResponse
		if !errors.As(err, &pe) || pe.Permission != want || pe.Repo != repo {
			t.Errorf("%s: %v, want permission %q", repo, err, want)
		}
		if !errors.Is(err, forge.ErrForbidden) || !errors.As(err, &ge) || strings.Contains(err.Error(), "ghu_test") {
			t.Errorf("%s: forbidden %v, chain kept %v, err %v", repo, errors.Is(err, forge.ErrForbidden), errors.As(err, &ge), err)
		}
	}
	for _, repo := range []string{"acme/plain", "acme/held"} {
		err := open(repo)
		var pe *forge.PermissionError
		if errors.As(err, &pe) || !errors.Is(err, forge.ErrForbidden) {
			t.Errorf("%s: %v, want a plain ErrForbidden", repo, err)
		}
	}
	_, err := c.SecretNames(ctx, "acme/two")
	var pe *forge.PermissionError
	if !errors.As(err, &pe) || pe.Permission != "Secrets: read" || !strings.Contains(err.Error(), "list secrets of acme/two") {
		t.Fatalf("SecretNames: %v", err)
	}
}

// The probe never follows a redirect: a redirect to a page that answers
// 200 would read as "can push".
func TestPushAccessNeverFollowsARedirect(t *testing.T) {
	t.Parallel()
	var followed atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			followed.Store(true)
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, "/login", http.StatusFound)
	}))
	defer srv.Close()
	c, err := ghforge.New("tok")
	if err != nil {
		t.Fatal(err)
	}
	if a := c.WithGitBase(srv.URL).PushAccess(context.Background(), "acme/widget"); a.Can != "unknown" || a.HTTPStatus != http.StatusFound || followed.Load() {
		t.Fatalf("redirect: %+v, followed %v", a, followed.Load())
	}
}

func TestProbePermissionsOnlyReads(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var methods []string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods = append(methods, r.Method)
		mu.Unlock()
		switch r.URL.Path {
		case "/repos/acme/widget-shadow":
			writeJSON(w, map[string]any{"full_name": "acme/widget-shadow", "private": true, "default_branch": "main"})
		case "/repos/acme/widget-shadow/commits", "/repos/acme/widget-shadow/pulls":
			writeJSON(w, []any{})
		case "/repos/acme/widget-shadow/actions/runs":
			w.Header().Set("X-Accepted-GitHub-Permissions", "actions=read")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message": "Resource not accessible by personal access token"}`))
		case "/repos/acme/widget-shadow/commits/main/status":
			writeJSON(w, map[string]any{"state": "success"})
		case "/repos/acme/widget-shadow/actions/secrets":
			writeJSON(w, map[string]any{"total_count": 0, "secrets": []any{}})
		case "/acme/widget-shadow.git/info/refs":
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c, err := ghforge.NewWithBaseURL("tok", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	c.WithGitBase(srv.URL)
	got, err := c.ProbePermissions(context.Background(), "acme/widget-shadow")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		forge.PermMetadataRead: forge.AccessYes, forge.PermContentsRead: forge.AccessYes, forge.PermPullsRead: forge.AccessYes,
		forge.PermActionsRead: forge.AccessNo, forge.PermStatusesRead: forge.AccessYes, forge.PermSecretsRead: forge.AccessYes,
		forge.PermContentsWrite: forge.AccessYes,
	}
	if !reflect.DeepEqual(got.Have, want) || got.Accepted[forge.PermActionsRead] != "actions=read" {
		t.Fatalf("got %+v", got)
	}
	for _, m := range methods {
		if m != http.MethodGet {
			t.Fatalf("the probe sent a %s", m)
		}
	}
	// A repo the token can't see: nothing more is probed.
	got, err = c.ProbePermissions(context.Background(), "acme/hidden")
	if err != nil || !reflect.DeepEqual(got.Have, map[string]string{forge.PermMetadataRead: forge.AccessNo}) {
		t.Fatalf("hidden: %+v %v", got, err)
	}
}

func TestOAuthScopes(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer tok" {
			w.Header().Set("X-OAuth-Scopes", "repo, workflow")
		}
		writeJSON(w, map[string]any{"login": "octo", "id": 42})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	classic, _ := ghforge.NewWithBaseURL("tok", srv.URL)
	scopes, ok, err := classic.OAuthScopes(context.Background())
	if err != nil || !ok || !reflect.DeepEqual(scopes, []string{"repo", "workflow"}) {
		t.Fatalf("classic: %v %v %v", scopes, ok, err)
	}
	fine, _ := ghforge.NewWithBaseURL("other", srv.URL)
	if scopes, ok, err := fine.OAuthScopes(context.Background()); err != nil || ok || scopes != nil {
		t.Fatalf("fine-grained: %v %v %v", scopes, ok, err)
	}
}
