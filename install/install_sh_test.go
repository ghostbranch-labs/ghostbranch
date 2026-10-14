package install

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// shRun is one run of install.sh in a fresh HOME and TMPDIR.
type shRun struct {
	home, tmp, out string
	err            error
}

// runInstallSh runs install.sh with a clean environment plus env.
func runInstallSh(t *testing.T, env ...string) shRun {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("install.sh is for macOS and Linux; the install.ps1 tests cover Windows")
	}
	r := shRun{home: t.TempDir(), tmp: t.TempDir()}
	cmd := exec.Command("sh", "install.sh")
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + r.home, "TMPDIR=" + r.tmp, "SHELL=/bin/bash"}, env...)
	out, err := cmd.CombinedOutput()
	r.out, r.err = string(out), err
	return r
}

func TestInstallShInstallsTheLatestReleaseIntoLocalBin(t *testing.T) {
	t.Parallel()
	s := twoReleases(t, nil)
	r := runInstallSh(t, "GHOSTBRANCH_DOWNLOAD_BASE="+s.URL)
	if r.err != nil {
		t.Fatalf("%v\n%s", r.err, r.out)
	}
	bin := filepath.Join(r.home, ".local", "bin", "ghostbranch")
	got, err := os.ReadFile(bin)
	if err != nil || !bytes.Equal(got, fakeBins["0.9.0"]) {
		t.Fatalf("%s isn't v0.9.0's binary (err %v)\n%s", bin, err, r.out)
	}
	if fi, err := os.Stat(bin); err != nil || fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("%s isn't executable: %v %v", bin, fi.Mode(), err)
	}
	for _, want := range []string{
		"Verified ghostbranch_0.9.0_",
		"Installed ghostbranch v0.9.0 (commit fake)",
		"at " + bin,
		"isn't on your PATH",
		`echo 'export PATH="` + filepath.Dir(bin) + `:$PATH"' >> ~/.bashrc`,
	} {
		if !strings.Contains(r.out, want) {
			t.Errorf("output lacks %q:\n%s", want, r.out)
		}
	}
	emptyDir(t, r.tmp)
}

func TestInstallShInstallsAPinnedVersion(t *testing.T) {
	t.Parallel()
	s := twoReleases(t, nil)
	for _, v := range []string{"v0.8.0", "0.8.0"} {
		dir := t.TempDir()
		r := runInstallSh(t, "GHOSTBRANCH_DOWNLOAD_BASE="+s.URL, "GHOSTBRANCH_INSTALL_DIR="+dir, "GHOSTBRANCH_VERSION="+v)
		if r.err != nil {
			t.Fatalf("%s: %v\n%s", v, r.err, r.out)
		}
		if got, _ := os.ReadFile(filepath.Join(dir, "ghostbranch")); !bytes.Equal(got, fakeBins["0.8.0"]) {
			t.Errorf("GHOSTBRANCH_VERSION=%s didn't install v0.8.0:\n%s", v, r.out)
		}
	}
	for _, p := range s.requests() {
		if strings.Contains(p, "latest") || strings.Contains(p, "0.9.0") {
			t.Errorf("a pinned install asked for %s", p)
		}
	}
}

// Spec § 7: a tampered download must be refused, and nothing installed.
func TestInstallShRefusesATamperedArchive(t *testing.T) {
	t.Parallel()
	s := twoReleases(t, tamper)
	dir := t.TempDir()
	r := runInstallSh(t, "GHOSTBRANCH_DOWNLOAD_BASE="+s.URL, "GHOSTBRANCH_INSTALL_DIR="+dir)
	if r.err == nil || !strings.Contains(r.out, "checksum mismatch") || !strings.Contains(r.out, "nothing was installed") {
		t.Fatalf("a tampered archive wasn't refused: %v\n%s", r.err, r.out)
	}
	emptyDir(t, dir)
	emptyDir(t, r.tmp)
}

func TestInstallShRefusesAnUnsupportedArch(t *testing.T) {
	t.Parallel()
	s := twoReleases(t, nil)
	for _, arch := range []string{"riscv64", "i686", "armv7l"} {
		r := runInstallSh(t, "GHOSTBRANCH_DOWNLOAD_BASE="+s.URL, "GHOSTBRANCH_INSTALL_DIR="+t.TempDir(), "GHOSTBRANCH_TEST_ARCH="+arch)
		if r.err == nil || !strings.Contains(r.out, "no build for") || !strings.Contains(r.out, "Supported: darwin_amd64, darwin_arm64, linux_amd64, linux_arm64") {
			t.Errorf("%s: %v\n%s", arch, r.err, r.out)
		}
	}
	if got := s.requests(); len(got) != 0 {
		t.Errorf("an unsupported arch still downloaded %v", got)
	}
}

func TestInstallShReplacesAnEarlierCopy(t *testing.T) {
	t.Parallel()
	s := twoReleases(t, nil)
	dir := t.TempDir()
	bin := filepath.Join(dir, "ghostbranch")
	if err := os.WriteFile(bin, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := runInstallSh(t, "GHOSTBRANCH_DOWNLOAD_BASE="+s.URL, "GHOSTBRANCH_INSTALL_DIR="+dir, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if r.err != nil {
		t.Fatalf("%v\n%s", r.err, r.out)
	}
	if got, _ := os.ReadFile(bin); !bytes.Equal(got, fakeBins["0.9.0"]) {
		t.Fatalf("the earlier copy wasn't replaced:\n%s", r.out)
	}
	if strings.Contains(r.out, "isn't on your PATH") {
		t.Errorf("printed a PATH line for a folder already on PATH:\n%s", r.out)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".ghostbranch.new*")); len(left) != 0 {
		t.Errorf("left %v behind", left)
	}
}

// Spec § 7: the test hook never allows plain http to a remote host.
func TestInstallShDownloadBaseIsHTTPSOrLocal(t *testing.T) {
	t.Parallel()
	for _, base := range []string{
		"http://example.com",
		"http://127.0.0.1.example.com",
		"http://localhost.example.com/x",
		"http://localhost@example.com",
		"http://localhost:80@example.com",
		"https://user@example.com",
		"ftp://127.0.0.1",
		"file:///etc",
	} {
		r := runInstallSh(t, "GHOSTBRANCH_DOWNLOAD_BASE="+base, "GHOSTBRANCH_INSTALL_DIR="+t.TempDir())
		if r.err == nil || !strings.Contains(r.out, "GHOSTBRANCH_DOWNLOAD_BASE must") {
			t.Errorf("%s was accepted: %v\n%s", base, r.err, r.out)
		}
	}
}

func hostTarget() string { return runtime.GOOS + "_" + runtime.GOARCH }

// editSums rewrites v0.9.0's checksums.txt line by line.
func editSums(f func(lines []string) []string) func(map[string][]byte) {
	return func(files map[string][]byte) {
		lines := strings.Split(strings.TrimSpace(string(files["v0.9.0/checksums.txt"])), "\n")
		files["v0.9.0/checksums.txt"] = []byte(strings.Join(f(lines), "\n") + "\n")
	}
}

// The latest install fetches only checksums.txt from latest/download; the
// archive comes from the tag that file names, so a release published
// between the two requests can't mix versions (R9).
func TestInstallShLatestReadsTheVersionFromChecksums(t *testing.T) {
	t.Parallel()
	s := twoReleases(t, nil)
	r := runInstallSh(t, "GHOSTBRANCH_DOWNLOAD_BASE="+s.URL, "GHOSTBRANCH_INSTALL_DIR="+t.TempDir())
	if r.err != nil {
		t.Fatalf("%v\n%s", r.err, r.out)
	}
	want := []string{
		"/latest/download/checksums.txt",
		"/download/v0.9.0/checksums.txt",
		"/download/v0.9.0/" + archiveName("0.9.0", hostTarget()),
	}
	if got := s.requests(); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("requests %v, want %v", got, want)
	}
}

func TestInstallShRefusesABadVersion(t *testing.T) {
	t.Parallel()
	s := twoReleases(t, nil)
	for _, v := range []string{"latest", "0.8.0/../v0.9.0", "v1.2", "1.2.3; rm -rf /"} {
		dir := t.TempDir()
		r := runInstallSh(t, "GHOSTBRANCH_DOWNLOAD_BASE="+s.URL, "GHOSTBRANCH_INSTALL_DIR="+dir, "GHOSTBRANCH_VERSION="+v)
		if r.err == nil || !strings.Contains(r.out, "isn't a version") {
			t.Errorf("%q was accepted: %v\n%s", v, r.err, r.out)
		}
		emptyDir(t, dir)
	}
	if got := s.requests(); len(got) != 0 {
		t.Errorf("a bad version still downloaded %v", got)
	}
}

func TestInstallShRefusesAMalformedChecksumsFile(t *testing.T) {
	t.Parallel()
	none := twoReleases(t, editSums(func(lines []string) []string {
		var keep []string
		for _, l := range lines {
			if !strings.Contains(l, hostTarget()) {
				keep = append(keep, l)
			}
		}
		return keep
	}))
	dup := twoReleases(t, editSums(func(lines []string) []string {
		for _, l := range lines {
			if strings.Contains(l, hostTarget()) {
				return append(lines, l)
			}
		}
		return lines
	}))
	for _, c := range []struct {
		s    *server
		want string
	}{{none, "no archive for"}, {dup, "more than one archive"}} {
		dir := t.TempDir()
		r := runInstallSh(t, "GHOSTBRANCH_DOWNLOAD_BASE="+c.s.URL, "GHOSTBRANCH_INSTALL_DIR="+dir)
		if r.err == nil || !strings.Contains(r.out, c.want) {
			t.Errorf("want %q: %v\n%s", c.want, r.err, r.out)
		}
		emptyDir(t, dir)
		emptyDir(t, r.tmp)
	}
}

// A stale staging file, even a link to somewhere else, is never written
// through, and none is left behind.
func TestInstallShIgnoresAStaleStagingFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("install.sh is for macOS and Linux")
	}
	t.Parallel()
	s := twoReleases(t, nil)
	dir, elsewhere := t.TempDir(), filepath.Join(t.TempDir(), "precious")
	if err := os.WriteFile(elsewhere, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(dir, ".ghostbranch.new")); err != nil {
		t.Fatal(err)
	}
	r := runInstallSh(t, "GHOSTBRANCH_DOWNLOAD_BASE="+s.URL, "GHOSTBRANCH_INSTALL_DIR="+dir)
	if r.err != nil {
		t.Fatalf("%v\n%s", r.err, r.out)
	}
	if got, _ := os.ReadFile(elsewhere); string(got) != "precious" {
		t.Errorf("the install wrote through a stale staging link: %q", got)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".ghostbranch.new*")); len(left) != 0 {
		t.Errorf("left %v behind", left)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "ghostbranch")); !bytes.Equal(got, fakeBins["0.9.0"]) {
		t.Errorf("not installed:\n%s", r.out)
	}
}

func TestInstallShWarnsWhenTheBinaryCannotRun(t *testing.T) {
	t.Parallel()
	s := twoReleases(t, func(files map[string][]byte) {
		name := "v0.9.0/" + archiveName("0.9.0", hostTarget())
		old := fmt.Sprintf("%x", sha256.Sum256(files[name]))
		files[name] = tarGzOf(t, "ghostbranch", []byte("#!/bin/sh\nexit 3\n"))
		sum := strings.Replace(string(files["v0.9.0/checksums.txt"]), old, fmt.Sprintf("%x", sha256.Sum256(files[name])), 1)
		files["v0.9.0/checksums.txt"] = []byte(sum)
	})
	r := runInstallSh(t, "GHOSTBRANCH_DOWNLOAD_BASE="+s.URL, "GHOSTBRANCH_INSTALL_DIR="+t.TempDir())
	if !strings.Contains(r.out, "doesn't run on this machine") || strings.Contains(r.out, "Installed ") {
		t.Errorf("a binary that can't run wasn't flagged:\n%s", r.out)
	}
}
