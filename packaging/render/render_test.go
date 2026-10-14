package main

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// The templates are the real ones in packaging/, one folder up.
func TestRenderMatchesTheGoldens(t *testing.T) {
	out := t.TempDir()
	var stdout bytes.Buffer
	if err := run([]string{"render", "-version", "v0.1.0", "-checksums", "testdata/checksums.txt", "-packaging", "..", "-out", out}, &stdout); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ghostbranch.rb", "ghostbranch.json"} {
		got, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			t.Fatal(err)
		}
		golden := filepath.Join("testdata", name+".golden")
		if *update {
			if err := os.WriteFile(golden, got, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from %s:\n%s", name, golden, got)
		}
	}
}

func TestRenderFailsWhenAnArchiveIsMissing(t *testing.T) {
	full, err := os.ReadFile("testdata/checksums.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, archive := range []string{
		"ghostbranch_0.1.0_darwin_arm64.tar.gz", "ghostbranch_0.1.0_darwin_amd64.tar.gz",
		"ghostbranch_0.1.0_linux_arm64.tar.gz", "ghostbranch_0.1.0_linux_amd64.tar.gz",
		"ghostbranch_0.1.0_windows_amd64.zip",
	} {
		var kept []string
		for _, line := range strings.Split(string(full), "\n") {
			if !strings.HasSuffix(line, "  "+archive) {
				kept = append(kept, line)
			}
		}
		_, err := newData("0.1.0", DefaultDownloadBase, strings.Join(kept, "\n"))
		if err == nil || !strings.Contains(err.Error(), archive) {
			t.Errorf("without %s: err %v, want one naming it", archive, err)
		}
	}
}

func TestRenderRefusesBadInput(t *testing.T) {
	full, err := os.ReadFile("testdata/checksums.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, version, base, sums, want string }{
		{"a quote in the version", `0.1.0"`, DefaultDownloadBase, string(full), "version"},
		{"not a version", "latest", DefaultDownloadBase, string(full), "version"},
		{"ruby interpolation in the base", "0.1.0", "https://example.com/#{x}", string(full), "download base"},
		{"not http", "0.1.0", "ftp://example.com", string(full), "download base"},
		{"user-info in the base", "0.1.0", "http://localhost@evil.example", string(full), "download base"},
		{"a short hash", "0.1.0", DefaultDownloadBase, strings.Replace(string(full), "4e4e4e4e", "4e", 1), "malformed SHA-256"},
		{"a duplicated archive", "0.1.0", DefaultDownloadBase, string(full) + "7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a  ghostbranch_0.1.0_linux_amd64.tar.gz\n", "ghostbranch_0.1.0_linux_amd64.tar.gz"},
	} {
		_, err := newData(c.version, c.base, c.sums)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want one containing %q", c.name, err, c.want)
		}
	}
}

func TestRenderNamesEveryMissingArchive(t *testing.T) {
	full, err := os.ReadFile("testdata/checksums.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, err = newData("0.2.0", DefaultDownloadBase, string(full))
	if err == nil {
		t.Fatal("no error")
	}
	for _, a := range []string{"darwin_arm64.tar.gz", "darwin_amd64.tar.gz", "linux_arm64.tar.gz", "linux_amd64.tar.gz", "windows_amd64.zip"} {
		if !strings.Contains(err.Error(), "ghostbranch_0.2.0_"+a) {
			t.Errorf("error %q doesn't name %s", err, a)
		}
	}
}

func TestRunRefusesBadCommandLines(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"no args", nil, "usage"},
		{"unknown command", []string{"frobnicate"}, "unknown command"},
		{"render without -checksums", []string{"render", "-version", "0.1.0", "-out", "x"}, "-checksums"},
		{"render without -out", []string{"render", "-version", "0.1.0", "-checksums", "testdata/checksums.txt"}, "-out"},
		{"route with a space-separated bool", []string{"route", "-tag", "v0.2.0-rc.1", "-prerelease", "false", "-stable-exists=true"}, "unexpected arguments"},
		{"render with a stray argument", []string{"render", "-version", "0.1.0", "-checksums", "testdata/checksums.txt", "-out", "x", "extra"}, "unexpected arguments"},
	} {
		err := run(c.args, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want one containing %q", c.name, err, c.want)
		}
	}
}

func TestRoute(t *testing.T) {
	for _, c := range []struct {
		tag         string
		pre, stable bool
		tap         bool
		npmTag      string
		bad         bool
	}{
		{tag: "v0.1.0", tap: true, npmTag: "latest"},
		{tag: "v0.2.0", stable: true, tap: true, npmTag: "latest"},
		{tag: "v0.1.0-rc.1", pre: true, tap: true, npmTag: "next"},                // the quiet window
		{tag: "v0.2.0-rc.1", pre: true, stable: true, tap: false, npmTag: "next"}, // after launch
		{tag: "v0.2.0-rc.1", stable: true, tap: false, npmTag: "next"},            // rc tag, box not ticked
		{tag: "v0.2.0", pre: true, stable: true, tap: false, npmTag: "next"},      // box ticked on a plain tag
		{tag: "v0.1.0", pre: true, tap: true, npmTag: "next"},                     // box ticked on a plain tag, before launch
		{tag: "0.2.0", bad: true},
		{tag: "v0.2.0-beta.1", bad: true},
		{tag: "v0.2", bad: true},
	} {
		r, err := Route(c.tag, c.pre, c.stable)
		if c.bad {
			if err == nil || !strings.Contains(err.Error(), "isn't vX.Y.Z") {
				t.Errorf("%s: err %v", c.tag, err)
			}
			continue
		}
		if err != nil || r.Tap != c.tap || r.NPMTag != c.npmTag || r.Version != strings.TrimPrefix(c.tag, "v") {
			t.Errorf("%s pre=%t stable=%t: %+v %v", c.tag, c.pre, c.stable, r, err)
		}
	}
}

func TestRouteCommandPrintsStepOutputs(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"route", "-tag", "v0.2.0-rc.1", "-prerelease=true", "-stable-exists=true"}, &out); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "version=0.2.0-rc.1\ntap=false\nnpm_tag=next\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
