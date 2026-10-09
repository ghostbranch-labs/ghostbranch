// Package workflow reads GitHub Actions workflow files: the repo guards,
// secrets and push triggers the CI audit reports, and the guard rewrite the
// gate applies to its CI candidate. It knows nothing of git or the forge.
package workflow

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Names are the pair's repos, owner/name each.
type Names struct{ Public, Shadow string }

func owner(full string) string { o, _, _ := strings.Cut(full, "/"); return o }

// Guard is one if: that compares the running repo with the public repo, so
// it skips in the shadow repo unless rewritten.
type Guard struct {
	Path       string  `json:"path"`
	Job        string  `json:"job"`
	Step       *string `json:"step"`
	Expr       string  `json:"expr"`
	Rewritable bool    `json:"rewritable"`
}

// GuardReport is what the gate did with a candidate's guards.
type GuardReport struct {
	Mode         string  `json:"mode"`
	Rewritten    []Guard `json:"rewritten"`
	Unrewritable []Guard `json:"unrewritable"`
	// Skipped are rewritable guards left as they are because
	// ci.rewrite_repo_guards is warn, so their jobs skip in the shadow repo
	// too. Nil in a plan written before 0.9.
	Skipped []Guard `json:"skipped"`
}

// File is what the CI audit needs from one workflow file.
type File struct {
	Path         string
	Guards       []Guard
	Secrets      []string
	Inherit      []string
	Environments []string
	// PushGuarded is true when every job has an if: holding the advised
	// push guard (github.event_name != 'push' || github.repository ==
	// '<public>'), so a push outside the public repo runs none of them.
	PushGuarded bool
	push        *pushFilter
}

// IsWorkflowPath reports whether GitHub Actions reads path as a workflow:
// a .yml or .yaml file directly under .github/workflows.
func IsWorkflowPath(path string) bool {
	rest, ok := strings.CutPrefix(path, ".github/workflows/")
	return ok && rest != "" && !strings.Contains(rest, "/") &&
		(strings.HasSuffix(rest, ".yml") || strings.HasSuffix(rest, ".yaml"))
}

func root(doc *yaml.Node) *yaml.Node {
	if doc.Kind == yaml.DocumentNode && len(doc.Content) == 1 && doc.Content[0].Kind == yaml.MappingNode {
		return doc.Content[0]
	}
	return nil
}

// get is m[key] for a mapping node, or nil.
func get(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func isString(n *yaml.Node) bool { return n != nil && n.Kind == yaml.ScalarNode && n.Tag == "!!str" }

// target is one if: value: a job's (step < 0) or a step's.
type target struct {
	job  string
	step int
	name *string
	node *yaml.Node
}

func targets(r *yaml.Node) []target {
	jobs := get(r, "jobs")
	if jobs == nil || jobs.Kind != yaml.MappingNode {
		return nil
	}
	var out []target
	for i := 0; i+1 < len(jobs.Content); i += 2 {
		id, job := jobs.Content[i].Value, jobs.Content[i+1]
		if n := get(job, "if"); isString(n) {
			out = append(out, target{job: id, step: -1, node: n})
		}
		steps := get(job, "steps")
		if steps == nil || steps.Kind != yaml.SequenceNode {
			continue
		}
		for j, st := range steps.Content {
			n := get(st, "if")
			if !isString(n) {
				continue
			}
			label := fmt.Sprintf("#%d", j+1)
			if v := get(st, "id"); isString(v) {
				label = v.Value
			} else if v := get(st, "name"); isString(v) {
				label = v.Value
			}
			out = append(out, target{job: id, step: j, name: &label, node: n})
		}
	}
	return out
}

var (
	secretDot   = regexp.MustCompile(`(?i)\bsecrets\.([A-Za-z_][A-Za-z0-9_]*)`)
	secretIndex = regexp.MustCompile(`(?i)\bsecrets\[\s*['"]([A-Za-z_][A-Za-z0-9_]*)['"]\s*\]`)
)

// Parse reads one workflow file. A file that is not valid YAML returns the
// parse error; GitHub reports such files itself.
func Parse(path string, src []byte, n Names) (File, error) {
	f := File{Path: path}
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return f, err
	}
	r := root(&doc)
	if r == nil {
		return f, nil
	}
	for _, t := range targets(r) {
		if _, guard, ok := guardExpr(t.node.Value, n); guard {
			f.Guards = append(f.Guards, Guard{Path: path, Job: t.job, Step: t.name, Expr: strings.TrimSpace(t.node.Value), Rewritable: ok})
		}
	}
	set := map[string]bool{}
	walk(r, func(s string) {
		for _, re := range []*regexp.Regexp{secretDot, secretIndex} {
			for _, m := range re.FindAllStringSubmatch(s, -1) {
				if name := strings.ToUpper(m[1]); name != "GITHUB_TOKEN" {
					set[name] = true
				}
			}
		}
	})
	for s := range set {
		f.Secrets = append(f.Secrets, s)
	}
	sort.Strings(f.Secrets)
	if jobs := get(r, "jobs"); jobs != nil && jobs.Kind == yaml.MappingNode {
		f.PushGuarded = len(jobs.Content) > 0
		for i := 0; i+1 < len(jobs.Content); i += 2 {
			id, job := jobs.Content[i].Value, jobs.Content[i+1]
			if cond := get(job, "if"); !isString(cond) || !IsPushSkip(cond.Value, n.Public) {
				f.PushGuarded = false
			}
			if s := get(job, "secrets"); s != nil && s.Kind == yaml.ScalarNode && s.Value == "inherit" {
				f.Inherit = append(f.Inherit, id)
			}
			if get(job, "environment") != nil {
				f.Environments = append(f.Environments, id)
			}
		}
	}
	f.push = parsePush(get(r, "on"))
	return f, nil
}

// walk calls fn with every scalar value under n; mapping keys are skipped.
func walk(n *yaml.Node, fn func(string)) {
	switch n.Kind {
	case yaml.ScalarNode:
		fn(n.Value)
	case yaml.MappingNode:
		for i := 1; i < len(n.Content); i += 2 {
			walk(n.Content[i], fn)
		}
	case yaml.SequenceNode, yaml.DocumentNode:
		for _, c := range n.Content {
			walk(c, fn)
		}
	}
}

// pushFilter is a workflow's on.push trigger.
type pushFilter struct {
	branches, ignore []string
	tagsOnly         bool
}

func parsePush(on *yaml.Node) *pushFilter {
	if on == nil {
		return nil
	}
	switch on.Kind {
	case yaml.ScalarNode:
		if on.Value == "push" {
			return &pushFilter{}
		}
	case yaml.SequenceNode:
		for _, c := range on.Content {
			if c.Kind == yaml.ScalarNode && c.Value == "push" {
				return &pushFilter{}
			}
		}
	case yaml.MappingNode:
		p := get(on, "push")
		if p == nil {
			return nil
		}
		f := &pushFilter{branches: strs(get(p, "branches")), ignore: strs(get(p, "branches-ignore"))}
		hasBranches := get(p, "branches") != nil || get(p, "branches-ignore") != nil
		f.tagsOnly = !hasBranches && (get(p, "tags") != nil || get(p, "tags-ignore") != nil)
		return f
	}
	return nil
}

func strs(n *yaml.Node) []string {
	switch {
	case n == nil:
		return nil
	case n.Kind == yaml.ScalarNode:
		return []string{n.Value}
	case n.Kind == yaml.SequenceNode:
		var out []string
		for _, c := range n.Content {
			if c.Kind == yaml.ScalarNode {
				out = append(out, c.Value)
			}
		}
		return out
	}
	return nil
}

// PushTriggered reports whether a push to any of branches starts this
// workflow. A pattern it can't evaluate exactly counts as a match, so the
// audit errs toward reporting.
func (f File) PushTriggered(branches []string) bool {
	p := f.push
	if p == nil || p.tagsOnly {
		return false
	}
	for _, b := range branches {
		switch {
		case len(p.branches) > 0:
			if anyMatch(p.branches, b) {
				return true
			}
		case len(p.ignore) > 0:
			if !anyMatch(p.ignore, b) {
				return true
			}
		default:
			return true
		}
	}
	return false
}

func anyMatch(patterns []string, s string) bool {
	for _, p := range patterns {
		if globMatch(p, s) {
			return true
		}
	}
	return false
}

// globMatch follows GitHub's filter patterns for * (not across /) and **;
// a pattern using ? + [ ] or ! counts as matching.
func globMatch(pattern, s string) bool {
	if strings.ContainsAny(pattern, "?+[]!") {
		return true
	}
	var re strings.Builder
	re.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		switch {
		case strings.HasPrefix(pattern[i:], "**"):
			re.WriteString(".*")
			i++
		case pattern[i] == '*':
			re.WriteString("[^/]*")
		default:
			re.WriteString(regexp.QuoteMeta(pattern[i : i+1]))
		}
	}
	re.WriteString("$")
	ok, _ := regexp.MatchString(re.String(), s)
	return ok
}
