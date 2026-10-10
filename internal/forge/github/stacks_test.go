package github_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	ghforge "github.com/ghostbranch-labs/ghostbranch/internal/forge/github"
)

var _ forge.Stacker = (*ghforge.Client)(nil)

// stackBody is a stack object as GitHub's /stacks endpoints return it.
func stackBody(number int, prs ...int) map[string]any {
	var entries []map[string]any
	for _, n := range prs {
		entries = append(entries, map[string]any{"number": n, "state": "open",
			"head": map[string]any{"ref": "h", "sha": "0123456789abcdef0123456789abcdef01234567"}})
	}
	return map[string]any{"id": 1980666, "number": number, "base": map[string]any{"ref": "main"}, "open": true, "pull_requests": entries}
}

func TestCreateAndExtendAStack(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	var bodies []string
	mux.HandleFunc("POST /repos/o/r/stacks", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, strings.TrimSpace(string(b)))
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, stackBody(4, 1, 2))
	})
	mux.HandleFunc("POST /repos/o/r/stacks/4/add", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, strings.TrimSpace(string(b)))
		writeJSON(w, stackBody(4, 1, 2, 3))
	})
	c := newClient(t, mux)
	ctx := context.Background()
	s, err := c.CreateStack(ctx, "o/r", []int{1, 2})
	if err != nil || s.Number != 4 || !slices.Equal(s.PRs, []int{1, 2}) {
		t.Fatalf("CreateStack = %+v %v", s, err)
	}
	s, err = c.AddToStack(ctx, "o/r", 4, []int{3})
	if err != nil || s.Number != 4 || !slices.Equal(s.PRs, []int{1, 2, 3}) {
		t.Fatalf("AddToStack = %+v %v", s, err)
	}
	if !slices.Equal(bodies, []string{`{"pull_requests":[1,2]}`, `{"pull_requests":[3]}`}) {
		t.Fatalf("request bodies = %q", bodies)
	}
}

// StackOf reads the PR's stack membership, then the stack; a PR outside any
// stack, or on a host without stacks, is nil.
func TestStackOf(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/pulls/2", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"number": 2, "state": "open", "head": map[string]any{"ref": "b"}, "base": map[string]any{"ref": "a"},
			"stack": map[string]any{"id": 1980666, "number": 4, "size": 3, "position": 2, "base": map[string]any{"ref": "main"}}})
	})
	mux.HandleFunc("GET /repos/o/r/pulls/5", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"number": 5, "state": "open", "head": map[string]any{"ref": "d"}, "base": map[string]any{"ref": "main"}})
	})
	mux.HandleFunc("GET /repos/o/r/stacks/4", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, stackBody(4, 1, 2, 3))
	})
	// PR 7 names stack 9, which dissolved before it was read.
	mux.HandleFunc("GET /repos/o/r/pulls/7", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"number": 7, "state": "open", "head": map[string]any{"ref": "g"}, "base": map[string]any{"ref": "main"},
			"stack": map[string]any{"id": 1980667, "number": 9, "size": 2, "position": 1, "base": map[string]any{"ref": "main"}}})
	})
	mux.HandleFunc("GET /repos/o/r/stacks/9", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})
	c := newClient(t, mux)
	ctx := context.Background()
	if s, err := c.StackOf(ctx, "o/r", 7); err != nil || s != nil {
		t.Fatalf("StackOf(7), stack dissolved = %+v %v, want nil", s, err)
	}
	s, err := c.StackOf(ctx, "o/r", 2)
	if err != nil || s == nil || s.Number != 4 || !slices.Equal(s.PRs, []int{1, 2, 3}) {
		t.Fatalf("StackOf(2) = %+v %v", s, err)
	}
	if s, err := c.StackOf(ctx, "o/r", 5); err != nil || s != nil {
		t.Fatalf("StackOf(5) = %+v %v, want nil", s, err)
	}
	if _, err := c.StackOf(ctx, "o/r", 6); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("StackOf(6), no such PR: %v", err)
	}
}

// 404 and 422 mean "no native stack here": the caller opens plain chained
// PRs. Nothing else does: a 409 is retried, a 403 is a permission, a 500
// is transient.
func TestStackErrors(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	fail := func(status int, msg string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { http.Error(w, `{"message":"`+msg+`"}`, status) }
	}
	mux.HandleFunc("POST /repos/o/nostacks/stacks", fail(http.StatusNotFound, "Not Found"))
	mux.HandleFunc("POST /repos/o/fork/stacks", fail(http.StatusUnprocessableEntity,
		"Pull requests must form a stack, where each PR's base ref is the previous PR's head ref"))
	mux.HandleFunc("POST /repos/o/busy/stacks/4/add", fail(http.StatusConflict, "A concurrent modification is in progress"))
	mux.HandleFunc("POST /repos/o/denied/stacks", fail(http.StatusForbidden, "Resource not accessible by personal access token"))
	mux.HandleFunc("POST /repos/o/down/stacks", fail(http.StatusInternalServerError, "boom"))
	c := newClient(t, mux)
	ctx := context.Background()

	_, err := c.CreateStack(ctx, "o/nostacks", []int{1, 2})
	if !errors.Is(err, forge.ErrStacksUnavailable) || errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("404: %v", err)
	}
	_, err = c.CreateStack(ctx, "o/fork", []int{2, 1})
	if !errors.Is(err, forge.ErrStacksUnavailable) || !strings.Contains(forge.HostMessage(err), "must form a stack") {
		t.Fatalf("422: %v (host message %q)", err, forge.HostMessage(err))
	}
	_, err = c.AddToStack(ctx, "o/busy", 4, []int{3})
	if !errors.Is(err, forge.ErrConflict) || errors.Is(err, forge.ErrStacksUnavailable) {
		t.Fatalf("409: %v", err)
	}
	_, err = c.CreateStack(ctx, "o/denied", []int{1, 2})
	if !errors.Is(err, forge.ErrForbidden) || errors.Is(err, forge.ErrStacksUnavailable) {
		t.Fatalf("403: %v", err)
	}
	_, err = c.CreateStack(ctx, "o/down", []int{1, 2})
	if !errors.Is(err, forge.ErrTransient) || errors.Is(err, forge.ErrStacksUnavailable) {
		t.Fatalf("500: %v", err)
	}
}
