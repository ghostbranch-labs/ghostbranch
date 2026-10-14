package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
)

// DefaultDownloadBase is where release files are downloaded from; a local
// test points -download-base at a local server instead.
const DefaultDownloadBase = "https://github.com/ghostbranch-labs/ghostbranch/releases/download"

var (
	// versionPattern is a release version without its v: X.Y.Z, then an
	// optional pre-release and build metadata (a snapshot's "+<commit>").
	versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)
	// tagPattern is the only tag shape that is distributed: vX.Y.Z or vX.Y.Z-rc.N.
	tagPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-rc\.(0|[1-9][0-9]*))?$`)
	shaPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Archive is one release archive: where to download it and its SHA-256.
type Archive struct {
	URL    string
	SHA256 string
}

// Data is what the templates are filled with.
type Data struct {
	Version      string // without the v
	DarwinARM64  Archive
	DarwinAMD64  Archive
	LinuxARM64   Archive
	LinuxAMD64   Archive
	WindowsAMD64 Archive
}

// newData reads the five archives' checksums for version from checksums
// (the release's checksums.txt). It fails when any archive is missing.
func newData(version, base, checksums string) (Data, error) {
	version = strings.TrimPrefix(version, "v")
	if !versionPattern.MatchString(version) {
		return Data{}, fmt.Errorf("version %q isn't X.Y.Z or X.Y.Z-pre", version)
	}
	if u, err := url.Parse(base); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil ||
		strings.ContainsAny(base, "\"'\\ \t\r\n#{}$`") {
		return Data{}, fmt.Errorf("download base %q isn't a plain http(s) URL", base)
	}
	sums := map[string]string{}
	for _, line := range strings.Split(checksums, "\n") {
		if f := strings.Fields(line); len(f) == 2 {
			name := strings.TrimPrefix(f[1], "*")
			if _, dup := sums[name]; dup {
				return Data{}, fmt.Errorf("checksums.txt lists %s twice", name)
			}
			sums[name] = f[0]
		}
	}
	var missing []string
	d := Data{Version: version}
	for _, a := range []struct {
		suffix string
		dst    *Archive
	}{
		{"darwin_arm64.tar.gz", &d.DarwinARM64},
		{"darwin_amd64.tar.gz", &d.DarwinAMD64},
		{"linux_arm64.tar.gz", &d.LinuxARM64},
		{"linux_amd64.tar.gz", &d.LinuxAMD64},
		{"windows_amd64.zip", &d.WindowsAMD64},
	} {
		name := "ghostbranch_" + version + "_" + a.suffix
		sum, ok := sums[name]
		if !ok {
			missing = append(missing, name)
			continue
		}
		if !shaPattern.MatchString(sum) {
			return Data{}, fmt.Errorf("checksums.txt has a malformed SHA-256 for %s", name)
		}
		*a.dst = Archive{URL: strings.TrimRight(base, "/") + "/v" + version + "/" + name, SHA256: sum}
	}
	if len(missing) > 0 {
		return Data{}, fmt.Errorf("checksums.txt has no line for %s", strings.Join(missing, ", "))
	}
	return d, nil
}

// parseFlags parses fs and rejects leftover positional arguments. Go's bool
// flags take only the -flag=value form, so "-prerelease false" would stop
// parsing at "false" and silently drop every flag after it.
func parseFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	return nil
}

// fill executes the template file at path with d.
func fill(path string, d Data) ([]byte, error) {
	t, err := template.New(filepath.Base(path)).ParseFiles(path)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err := t.Execute(&b, d); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// Routing is where a published release goes.
type Routing struct {
	Version string // without the v
	Tap     bool   // push to the Homebrew tap and the Scoop bucket
	NPMTag  string // the npm dist-tag: "latest" or "next"
}

// Route decides where a published release goes (spec § 2). A release is a
// test build when its tag has -rc.N or it is marked pre-release; either is
// enough, so a wrongly ticked box never sends a test build to brew or scoop
// users. A test build reaches the tap and bucket only while no stable
// release exists (the quiet window before v0.1.0).
func Route(tag string, prerelease, stableExists bool) (Routing, error) {
	m := tagPattern.FindStringSubmatch(tag)
	if m == nil {
		return Routing{}, fmt.Errorf("tag %q isn't vX.Y.Z or vX.Y.Z-rc.N", tag)
	}
	r := Routing{Version: strings.TrimPrefix(tag, "v")}
	switch test := m[4] != "" || prerelease; {
	case !test:
		r.Tap, r.NPMTag = true, "latest"
	case !stableExists:
		r.Tap, r.NPMTag = true, "next"
	default:
		r.Tap, r.NPMTag = false, "next"
	}
	return r, nil
}

func run(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: render render|route [flags]")
	}
	switch args[0] {
	case "render":
		fs := flag.NewFlagSet("render", flag.ContinueOnError)
		version := fs.String("version", "", "the release version, with or without its v")
		sums := fs.String("checksums", "", "the release's checksums.txt")
		dir := fs.String("packaging", "packaging", "the folder holding homebrew/ and scoop/")
		base := fs.String("download-base", DefaultDownloadBase, "the release download base URL")
		out := fs.String("out", "", "the folder to write ghostbranch.rb and ghostbranch.json into")
		if err := parseFlags(fs, args[1:]); err != nil {
			return err
		}
		if *sums == "" || *out == "" {
			return errors.New("render needs -checksums and -out")
		}
		raw, err := os.ReadFile(*sums)
		if err != nil {
			return err
		}
		d, err := newData(*version, *base, string(raw))
		if err != nil {
			return err
		}
		if err := os.MkdirAll(*out, 0o755); err != nil {
			return err
		}
		for _, f := range []struct{ tmpl, name string }{
			{filepath.Join("homebrew", "ghostbranch.rb.tmpl"), "ghostbranch.rb"},
			{filepath.Join("scoop", "ghostbranch.json.tmpl"), "ghostbranch.json"},
		} {
			b, err := fill(filepath.Join(*dir, f.tmpl), d)
			if err != nil {
				return err
			}
			if strings.HasSuffix(f.name, ".json") && !json.Valid(b) {
				return fmt.Errorf("%s isn't valid JSON", f.name)
			}
			if err := os.WriteFile(filepath.Join(*out, f.name), b, 0o644); err != nil {
				return err
			}
		}
		_, err = fmt.Fprintf(stdout, "rendered ghostbranch.rb and ghostbranch.json for %s into %s\n", d.Version, *out)
		return err
	case "route":
		fs := flag.NewFlagSet("route", flag.ContinueOnError)
		tag := fs.String("tag", "", "the release's tag")
		pre := fs.Bool("prerelease", false, "the release is marked pre-release")
		stable := fs.Bool("stable-exists", false, "a published release that isn't a pre-release exists")
		if err := parseFlags(fs, args[1:]); err != nil {
			return err
		}
		r, err := Route(*tag, *pre, *stable)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "version=%s\ntap=%t\nnpm_tag=%s\n", r.Version, r.Tap, r.NPMTag)
		return err
	}
	return fmt.Errorf("unknown command %q: want render or route", args[0])
}
