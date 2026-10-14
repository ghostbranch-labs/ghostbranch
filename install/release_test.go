// Package install tests install.sh and install.ps1 against a fake release
// served from a local server: the same archive names and checksums.txt as
// GoReleaser's, with a fake ghostbranch inside the host's archive.
package install

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// fakeBins is the fake ghostbranch built for this machine, by version.
var fakeBins = map[string][]byte{}

var exeSuffix = map[bool]string{true: ".exe", false: ""}[runtime.GOOS == "windows"]

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "ghostbranch-install-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	goTool, err := exec.LookPath("go")
	if err != nil {
		fmt.Fprintln(os.Stderr, "the install tests need the go tool on PATH:", err)
		os.Exit(1)
	}
	for _, v := range []string{"0.8.0", "0.9.0"} {
		out := filepath.Join(dir, "fake-"+v+exeSuffix)
		build := exec.Command(goTool, "build", "-o", out, "-ldflags", "-X main.version=v"+v, "./testdata/fakebin")
		if b, err := build.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "build the fake binary: %v\n%s", err, b)
			os.Exit(1)
		}
		if fakeBins[v], err = os.ReadFile(out); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// targets are the five release targets, as in .goreleaser.yaml.
var targets = []string{"darwin_amd64", "darwin_arm64", "linux_amd64", "linux_arm64", "windows_amd64"}

// archiveName is GoReleaser's archive name for version v and target.
func archiveName(v, target string) string {
	if strings.HasPrefix(target, "windows_") {
		return "ghostbranch_" + v + "_" + target + ".zip"
	}
	return "ghostbranch_" + v + "_" + target + ".tar.gz"
}

// addRelease adds release v to files, keyed "v<version>/<file>": five
// archives (the host's holds the fake binary, the rest a stub) and a
// checksums.txt listing them.
func addRelease(t *testing.T, files map[string][]byte, v string) {
	t.Helper()
	var sums strings.Builder
	for _, target := range targets {
		bin := []byte("stub")
		if target == runtime.GOOS+"_"+runtime.GOARCH {
			bin = fakeBins[v]
		}
		name := archiveName(v, target)
		var data []byte
		if strings.HasPrefix(target, "windows_") {
			data = zipOf(t, "ghostbranch.exe", bin)
		} else {
			data = tarGzOf(t, "ghostbranch", bin)
		}
		files["v"+v+"/"+name] = data
		fmt.Fprintf(&sums, "%x  %s\n", sha256.Sum256(data), name)
	}
	files["v"+v+"/checksums.txt"] = []byte(sums.String())
}

func tarGzOf(t *testing.T, name string, bin []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for _, f := range []struct {
		name string
		mode int64
		data []byte
	}{{name, 0o755, bin}, {"LICENSE", 0o644, []byte("license")}, {"README.md", 0o644, []byte("readme")}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(f.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func zipOf(t *testing.T, name string, bin []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for _, f := range []struct {
		name string
		data []byte
	}{{name, bin}, {"LICENSE", []byte("license")}, {"README.md", []byte("readme")}} {
		w, err := zw.Create(f.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(f.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// server serves files like GitHub's release downloads: download/<tag>/<file>,
// and latest/download/<file> redirecting to the latest tag's file. It
// records each path asked for.
type server struct {
	URL  string
	mu   sync.Mutex
	hits []string
}

func serve(t *testing.T, files map[string][]byte, latest string) *server {
	t.Helper()
	s := &server{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.hits = append(s.hits, r.URL.Path)
		s.mu.Unlock()
		if rest, ok := strings.CutPrefix(r.URL.Path, "/latest/download/"); ok {
			http.Redirect(w, r, "/download/"+latest+"/"+rest, http.StatusFound)
			return
		}
		b, ok := files[strings.TrimPrefix(r.URL.Path, "/download/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	s.URL = srv.URL
	return s
}

func (s *server) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.hits...)
}

// twoReleases serves v0.8.0 and v0.9.0, with v0.9.0 the latest. edit, when
// set, changes the files before the server starts.
func twoReleases(t *testing.T, edit func(map[string][]byte)) *server {
	t.Helper()
	files := map[string][]byte{}
	addRelease(t, files, "0.8.0")
	addRelease(t, files, "0.9.0")
	if edit != nil {
		edit(files)
	}
	return serve(t, files, "v0.9.0")
}

// tamper flips the last byte of the host's v0.9.0 archive, after its
// checksum was written.
func tamper(files map[string][]byte) {
	name := "v0.9.0/" + archiveName("0.9.0", runtime.GOOS+"_"+runtime.GOARCH)
	b := append([]byte(nil), files[name]...)
	b[len(b)-1] ^= 0xff
	files[name] = b
}

func emptyDir(t *testing.T, dir string) {
	t.Helper()
	left, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("%s still holds %d entries; the script must clean up its downloads", dir, len(left))
	}
}
