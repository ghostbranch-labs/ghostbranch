package install

import (
	"bytes"
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
