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

// powershells is every PowerShell on this machine: Windows PowerShell 5.1
// (powershell) and PowerShell 7 (pwsh). Each test runs under each.
func powershells(t *testing.T) []string {
	t.Helper()
	var found []string
	for _, name := range []string{"powershell", "pwsh"} {
		if p, err := exec.LookPath(name); err == nil {
			found = append(found, p)
		}
	}
	if len(found) == 0 {
		t.Skip("no PowerShell on this machine (powershell or pwsh); install.ps1 isn't tested here")
	}
	return found
}

// psRun is one run of install.ps1.
type psRun struct {
	tmp, out string
	err      error
}

// runInstallPs1 runs install.ps1 under shell with this process's
// environment (minus any GHOSTBRANCH_ entries) plus env. TEMP and TMP point at a fresh folder, and the user
// PATH is never written (GHOSTBRANCH_TEST_SKIP_PATH).
func runInstallPs1(t *testing.T, shell string, env ...string) psRun {
	t.Helper()
	r := psRun{tmp: t.TempDir()}
	cmd := exec.Command(shell, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", "install.ps1")
	var base []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(e), "GHOSTBRANCH_") {
			base = append(base, e)
		}
	}
	cmd.Env = append(append(base, "TEMP="+r.tmp, "TMP="+r.tmp, "TMPDIR="+r.tmp, "GHOSTBRANCH_TEST_SKIP_PATH=1"), env...)
	out, err := cmd.CombinedOutput()
	r.out, r.err = string(out), err
	return r
}

// windowsOnly skips on macOS and Linux, where install.ps1 only refuses
// (TestInstallPs1RefusesOutsideWindows).
func windowsOnly(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("install.ps1 installs on Windows only")
	}
}

func TestInstallPs1InstallsTheLatestRelease(t *testing.T) {
	t.Parallel()
	windowsOnly(t)
	s := twoReleases(t, nil)
	for _, shell := range powershells(t) {
		local := t.TempDir()
		r := runInstallPs1(t, shell, "GHOSTBRANCH_DOWNLOAD_BASE="+s.URL, "LOCALAPPDATA="+local)
		if r.err != nil {
			t.Fatalf("%s: %v\n%s", shell, r.err, r.out)
		}
		exe := filepath.Join(local, "Programs", "ghostbranch", "ghostbranch.exe")
		if got, _ := os.ReadFile(exe); !bytes.Equal(got, fakeBins["0.9.0"]) {
			t.Fatalf("%s: %s isn't v0.9.0's binary\n%s", shell, exe, r.out)
		}
		for _, want := range []string{"Verified ghostbranch_0.9.0_windows_amd64.zip", "Installed ghostbranch v0.9.0 (commit fake)", "Would add " + filepath.Dir(exe) + " to the user PATH."} {
			if !strings.Contains(r.out, want) {
				t.Errorf("%s: output lacks %q:\n%s", shell, want, r.out)
			}
		}
		emptyDir(t, r.tmp)
	}
}

func TestInstallPs1InstallsAPinnedVersion(t *testing.T) {
	t.Parallel()
	windowsOnly(t)
	s := twoReleases(t, nil)
	for _, shell := range powershells(t) {
		dir := t.TempDir()
		r := runInstallPs1(t, shell, "GHOSTBRANCH_DOWNLOAD_BASE="+s.URL, "GHOSTBRANCH_INSTALL_DIR="+dir, "GHOSTBRANCH_VERSION=v0.8.0")
		if r.err != nil {
			t.Fatalf("%s: %v\n%s", shell, r.err, r.out)
		}
		if got, _ := os.ReadFile(filepath.Join(dir, "ghostbranch.exe")); !bytes.Equal(got, fakeBins["0.8.0"]) {
			t.Errorf("%s: didn't install v0.8.0:\n%s", shell, r.out)
		}
	}
}

// Spec § 7: a tampered download must be refused, and nothing installed.
func TestInstallPs1RefusesATamperedArchive(t *testing.T) {
	t.Parallel()
	windowsOnly(t)
	s := twoReleases(t, tamper)
	for _, shell := range powershells(t) {
		dir := t.TempDir()
		r := runInstallPs1(t, shell, "GHOSTBRANCH_DOWNLOAD_BASE="+s.URL, "GHOSTBRANCH_INSTALL_DIR="+dir)
		if r.err == nil || !strings.Contains(r.out, "checksum mismatch") {
			t.Fatalf("%s: a tampered archive wasn't refused: %v\n%s", shell, r.err, r.out)
		}
		emptyDir(t, dir)
		emptyDir(t, r.tmp)
	}
}

func TestInstallPs1RefusesAnUnsupportedArch(t *testing.T) {
	t.Parallel()
	windowsOnly(t)
	s := twoReleases(t, nil)
	for _, shell := range powershells(t) {
		for arch, want := range map[string]string{
			"X86":   "no build for Windows X86",
			"ARM64": "set $env:GHOSTBRANCH_ARCH = 'amd64'",
		} {
			r := runInstallPs1(t, shell, "GHOSTBRANCH_DOWNLOAD_BASE="+s.URL, "GHOSTBRANCH_INSTALL_DIR="+t.TempDir(), "GHOSTBRANCH_TEST_ARCH="+arch)
			if r.err == nil || !strings.Contains(r.out, want) || !strings.Contains(r.out, "Supported: windows_amd64") {
				t.Errorf("%s %s: %v\n%s", shell, arch, r.err, r.out)
			}
		}
		// Windows on ARM installs the amd64 build only when asked to.
		dir := t.TempDir()
		r := runInstallPs1(t, shell, "GHOSTBRANCH_DOWNLOAD_BASE="+s.URL, "GHOSTBRANCH_INSTALL_DIR="+dir, "GHOSTBRANCH_TEST_ARCH=ARM64", "GHOSTBRANCH_ARCH=amd64")
		if got, _ := os.ReadFile(filepath.Join(dir, "ghostbranch.exe")); r.err != nil || !bytes.Equal(got, fakeBins["0.9.0"]) {
			t.Errorf("%s: ARM64 with GHOSTBRANCH_ARCH=amd64: %v\n%s", shell, r.err, r.out)
		}
	}
}

func TestInstallPs1ReplacesAnEarlierCopy(t *testing.T) {
	t.Parallel()
	windowsOnly(t)
	s := twoReleases(t, nil)
	for _, shell := range powershells(t) {
		dir := t.TempDir()
		exe := filepath.Join(dir, "ghostbranch.exe")
		if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
			t.Fatal(err)
		}
		r := runInstallPs1(t, shell, "GHOSTBRANCH_DOWNLOAD_BASE="+s.URL, "GHOSTBRANCH_INSTALL_DIR="+dir)
		if got, _ := os.ReadFile(exe); r.err != nil || !bytes.Equal(got, fakeBins["0.9.0"]) {
			t.Fatalf("%s: the earlier copy wasn't replaced: %v\n%s", shell, r.err, r.out)
		}
		if _, err := os.Stat(exe + ".old"); err == nil {
			t.Errorf("%s: left ghostbranch.exe.old behind", shell)
		}
	}
}

// Spec § 7: the test hook never allows plain http to a remote host.
func TestInstallPs1DownloadBaseIsHTTPSOrLocal(t *testing.T) {
	t.Parallel()
	windowsOnly(t)
	for _, shell := range powershells(t) {
		for _, base := range []string{
			"http://example.com",
			"http://127.0.0.1.example.com",
			"http://localhost.example.com/x",
			"http://localhost@example.com",
			"http://localhost:80@example.com",
			"https://user@example.com",
			"ftp://127.0.0.1",
			"file:///C:/Windows",
		} {
			r := runInstallPs1(t, shell, "GHOSTBRANCH_DOWNLOAD_BASE="+base, "GHOSTBRANCH_INSTALL_DIR="+t.TempDir())
			if r.err == nil || !strings.Contains(r.out, "GHOSTBRANCH_DOWNLOAD_BASE must") {
				t.Errorf("%s: %s was accepted: %v\n%s", shell, base, r.err, r.out)
			}
		}
	}
}

func TestInstallPs1RefusesOutsideWindows(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("covered by the install tests above")
	}
	for _, shell := range powershells(t) {
		r := runInstallPs1(t, shell, "GHOSTBRANCH_INSTALL_DIR="+t.TempDir())
		if r.err == nil || !strings.Contains(r.out, "use install.sh") {
			t.Errorf("%s: %v\n%s", shell, r.err, r.out)
		}
	}
}

func TestInstallPs1RefusesABadVersion(t *testing.T) {
	t.Parallel()
	windowsOnly(t)
	s := twoReleases(t, nil)
	for _, shell := range powershells(t) {
		for _, v := range []string{"latest", "0.8.0/../v0.9.0", "v1.2", "1.2.3; calc"} {
			dir := t.TempDir()
			r := runInstallPs1(t, shell, "GHOSTBRANCH_DOWNLOAD_BASE="+s.URL, "GHOSTBRANCH_INSTALL_DIR="+dir, "GHOSTBRANCH_VERSION="+v)
			if r.err == nil || !strings.Contains(r.out, "isn't a version") {
				t.Errorf("%s: %q was accepted: %v\n%s", shell, v, r.err, r.out)
			}
			emptyDir(t, dir)
			emptyDir(t, r.tmp)
		}
	}
	if got := s.requests(); len(got) != 0 {
		t.Errorf("a bad version still downloaded %v", got)
	}
}

// The version in checksums.txt is checked too, before it reaches a URL or a path.
func TestInstallPs1RefusesABadVersionInChecksums(t *testing.T) {
	t.Parallel()
	windowsOnly(t)
	s := twoReleases(t, editSums(func(lines []string) []string {
		for i, l := range lines {
			if strings.Contains(l, hostTarget()) {
				lines[i] = strings.Replace(l, "ghostbranch_0.9.0_", "ghostbranch_9;calc_", 1)
			}
		}
		return lines
	}))
	for _, shell := range powershells(t) {
		dir := t.TempDir()
		r := runInstallPs1(t, shell, "GHOSTBRANCH_DOWNLOAD_BASE="+s.URL, "GHOSTBRANCH_INSTALL_DIR="+dir)
		if r.err == nil || !strings.Contains(r.out, "isn't a version") {
			t.Errorf("%s: a bad version in checksums.txt was accepted: %v\n%s", shell, r.err, r.out)
		}
		emptyDir(t, dir)
		emptyDir(t, r.tmp)
	}
}

// A stale staging file or earlier .old is replaced, and none is left behind.
func TestInstallPs1IgnoresAStaleStagingFile(t *testing.T) {
	t.Parallel()
	windowsOnly(t)
	s := twoReleases(t, nil)
	for _, shell := range powershells(t) {
		dir := t.TempDir()
		for _, n := range []string{"ghostbranch.exe.new", "ghostbranch.exe.old"} {
			if err := os.WriteFile(filepath.Join(dir, n), []byte("stale"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		r := runInstallPs1(t, shell, "GHOSTBRANCH_DOWNLOAD_BASE="+s.URL, "GHOSTBRANCH_INSTALL_DIR="+dir)
		if r.err != nil {
			t.Fatalf("%s: %v\n%s", shell, r.err, r.out)
		}
		if got, _ := os.ReadFile(filepath.Join(dir, "ghostbranch.exe")); !bytes.Equal(got, fakeBins["0.9.0"]) {
			t.Errorf("%s: not installed:\n%s", shell, r.out)
		}
		if left, _ := filepath.Glob(filepath.Join(dir, "ghostbranch.exe.*")); len(left) != 0 {
			t.Errorf("%s: left %v behind", shell, left)
		}
	}
}

func TestInstallPs1WarnsWhenTheBinaryCannotRun(t *testing.T) {
	t.Parallel()
	windowsOnly(t)
	s := twoReleases(t, func(files map[string][]byte) {
		name := "v0.9.0/" + archiveName("0.9.0", hostTarget())
		old := fmt.Sprintf("%x", sha256.Sum256(files[name]))
		files[name] = zipOf(t, "ghostbranch.exe", []byte("this is not a program"))
		sum := strings.Replace(string(files["v0.9.0/checksums.txt"]), old, fmt.Sprintf("%x", sha256.Sum256(files[name])), 1)
		files["v0.9.0/checksums.txt"] = []byte(sum)
	})
	for _, shell := range powershells(t) {
		r := runInstallPs1(t, shell, "GHOSTBRANCH_DOWNLOAD_BASE="+s.URL, "GHOSTBRANCH_INSTALL_DIR="+t.TempDir())
		if !strings.Contains(r.out, "doesn't run on this machine") || strings.Contains(r.out, "Installed ") {
			t.Errorf("%s: a binary that can't run wasn't flagged:\n%s", shell, r.out)
		}
	}
}

// pathKey is a throwaway registry key under HKCU standing in for HKCU\Environment
// (GHOSTBRANCH_TEST_PATH_KEY). setRaw, when set, seeds its Path value with
// that kind. The key is deleted when the test ends.
type pathKey struct {
	shell, sub string
}

func newPathKey(t *testing.T, shell, kind, setRaw string) pathKey {
	t.Helper()
	k := pathKey{shell, `Software\ghostbranch-test-` + strings.ReplaceAll(t.Name(), "/", "_") + fmt.Sprint(os.Getpid(), "-", len(t.TempDir()), "-", filepath.Base(t.TempDir()))}
	t.Cleanup(func() {
		exec.Command(shell, "-NoProfile", "-NonInteractive", "-Command", `Remove-Item -Recurse -Force -ErrorAction SilentlyContinue 'HKCU:\`+k.sub+`'`).Run()
	})
	script := `New-Item -Force 'HKCU:\` + k.sub + `' | Out-Null`
	if kind != "" {
		script += `; $k = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('` + k.sub + `', $true); $k.SetValue('Path', '` + setRaw + `', [Microsoft.Win32.RegistryValueKind]::` + kind + `); $k.Dispose()`
	}
	if out, err := exec.Command(shell, "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return k
}

// read returns the key's Path value, unexpanded, and its kind ("" if missing).
func (k pathKey) read(t *testing.T) (kind, raw string) {
	t.Helper()
	script := `$k = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('` + k.sub + `'); if (@($k.GetValueNames()) -contains 'Path') { $k.GetValueKind('Path').ToString() + '|' + $k.GetValue('Path', '', 'DoNotExpandEnvironmentNames') }; $k.Dispose()`
	out, err := exec.Command(k.shell, "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	kind, raw, _ = strings.Cut(strings.TrimSpace(string(out)), "|")
	return kind, raw
}

func TestInstallPs1EditsTheUserPathRaw(t *testing.T) {
	t.Parallel()
	windowsOnly(t)
	s := twoReleases(t, nil)
	for _, shell := range powershells(t) {
		dir := t.TempDir()
		k := newPathKey(t, shell, "ExpandString", `%USERPROFILE%\x;;C:\y`)
		r := runInstallPs1(t, shell, "GHOSTBRANCH_DOWNLOAD_BASE="+s.URL, "GHOSTBRANCH_INSTALL_DIR="+dir, "GHOSTBRANCH_TEST_SKIP_PATH=", "GHOSTBRANCH_TEST_PATH_KEY="+k.sub)
		if r.err != nil {
			t.Fatalf("%s: %v\n%s", shell, r.err, r.out)
		}
		if kind, raw := k.read(t); kind != "ExpandString" || raw != `%USERPROFILE%\x;;C:\y;`+dir {
			t.Errorf("%s: Path is %s %q\n%s", shell, kind, raw, r.out)
		}
	}
}

func TestInstallPs1DoesNotDuplicateAPathEntry(t *testing.T) {
	t.Parallel()
	windowsOnly(t)
	s := twoReleases(t, nil)
	for _, shell := range powershells(t) {
		dir := t.TempDir()
		before := `C:\a;` + strings.ToUpper(dir) + `\;C:\b`
		k := newPathKey(t, shell, "String", before)
		r := runInstallPs1(t, shell, "GHOSTBRANCH_DOWNLOAD_BASE="+s.URL, "GHOSTBRANCH_INSTALL_DIR="+dir, "GHOSTBRANCH_TEST_SKIP_PATH=", "GHOSTBRANCH_TEST_PATH_KEY="+k.sub)
		if r.err != nil {
			t.Fatalf("%s: %v\n%s", shell, r.err, r.out)
		}
		if kind, raw := k.read(t); kind != "String" || raw != before {
			t.Errorf("%s: Path is %s %q, want it unchanged\n%s", shell, kind, raw, r.out)
		}
	}
}

func TestInstallPs1CreatesAMissingUserPath(t *testing.T) {
	t.Parallel()
	windowsOnly(t)
	s := twoReleases(t, nil)
	for _, shell := range powershells(t) {
		dir := t.TempDir()
		k := newPathKey(t, shell, "", "")
		r := runInstallPs1(t, shell, "GHOSTBRANCH_DOWNLOAD_BASE="+s.URL, "GHOSTBRANCH_INSTALL_DIR="+dir, "GHOSTBRANCH_TEST_SKIP_PATH=", "GHOSTBRANCH_TEST_PATH_KEY="+k.sub)
		if r.err != nil {
			t.Fatalf("%s: %v\n%s", shell, r.err, r.out)
		}
		if kind, raw := k.read(t); kind != "ExpandString" || raw != dir {
			t.Errorf("%s: Path is %s %q, want ExpandString %q\n%s", shell, kind, raw, dir, r.out)
		}
	}
}

func TestInstallPs1TestPathKeyMustBeAThrowawayKey(t *testing.T) {
	t.Parallel()
	windowsOnly(t)
	for _, shell := range powershells(t) {
		// The key is refused up front, before any download: the base is a
		// closed local port, so a guard that ran later would fail on the
		// download instead, with a different message.
		r := runInstallPs1(t, shell, "GHOSTBRANCH_TEST_PATH_KEY=Environment", "GHOSTBRANCH_DOWNLOAD_BASE=http://127.0.0.1:1", "GHOSTBRANCH_INSTALL_DIR="+t.TempDir())
		if r.err == nil || !strings.Contains(r.out, `must start with Software\ghostbranch-test-`) {
			t.Errorf("%s: accepted HKCU Environment as the test key: %v\n%s", shell, r.err, r.out)
		}
		if strings.Contains(r.out, "couldn't download") {
			t.Errorf("%s: downloaded before refusing the test key\n%s", shell, r.out)
		}
	}
}

func TestInstallPs1RefusesAMalformedChecksumsFile(t *testing.T) {
	t.Parallel()
	windowsOnly(t)
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
	for _, shell := range powershells(t) {
		for _, c := range []struct {
			s    *server
			want string
		}{{none, "lists 0 archives"}, {dup, "lists 2 archives"}} {
			dir := t.TempDir()
			r := runInstallPs1(t, shell, "GHOSTBRANCH_DOWNLOAD_BASE="+c.s.URL, "GHOSTBRANCH_INSTALL_DIR="+dir)
			if r.err == nil || !strings.Contains(r.out, c.want) {
				t.Errorf("%s: want %q: %v\n%s", shell, c.want, r.err, r.out)
			}
			emptyDir(t, dir)
			emptyDir(t, r.tmp)
		}
	}
}
