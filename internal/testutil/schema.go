package testutil

import (
	"bytes"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// ValidateSchema fails t unless raw matches schemas/v0/<name>, with formats
// asserted. It finds the schemas from this file's own path, so any package's
// tests can call it.
func ValidateSchema(t testing.TB, name string, raw []byte) {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("can't locate schemas/v0")
	}
	path := filepath.Join(filepath.Dir(here), "..", "..", "schemas", "v0", name)
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	s, err := c.Compile(path)
	if err != nil {
		t.Fatalf("compile %s: %v", name, err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("not JSON (%v):\n%s", err, raw)
	}
	if err := s.Validate(inst); err != nil {
		t.Fatalf("does not match %s:\n%v\n%s", name, err, raw)
	}
}
