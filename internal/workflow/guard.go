package workflow

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// subject is what a guard compares. repository_owner comes first so the
// alternation never stops at the shorter github.repository.
const subject = `(github\.repository_owner|github\.event\.repository\.full_name|github\.repository)`

var (
	subjectFirst = regexp.MustCompile(`(?i)\b` + subject + `\s*(==|!=)\s*'((?:[^']|'')*)'`)
	literalFirst = regexp.MustCompile(`(?i)'((?:[^']|'')*)'\s*(==|!=)\s*\b` + subject + `\b`)
)

// cmp is one comparison of a subject with a string literal in an expression.
type cmp struct {
	start, end           int
	subject, op, literal string
}

func comparisons(expr string) []cmp {
	var all []cmp
	for _, m := range subjectFirst.FindAllStringSubmatchIndex(expr, -1) {
		all = append(all, cmp{m[0], m[1], expr[m[2]:m[3]], expr[m[4]:m[5]], strings.ReplaceAll(expr[m[6]:m[7]], "''", "'")})
	}
	for _, m := range literalFirst.FindAllStringSubmatchIndex(expr, -1) {
		all = append(all, cmp{m[0], m[1], expr[m[6]:m[7]], expr[m[4]:m[5]], strings.ReplaceAll(expr[m[2]:m[3]], "''", "'")})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].start < all[j].start })
	var out []cmp
	for _, c := range all {
		if len(out) == 0 || c.start >= out[len(out)-1].end {
			out = append(out, c)
		}
	}
	return out
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// widen rewrites c so it also matches the shadow repo, keeping its meaning
// for every other repo.
func widen(c cmp, shadowValue string) string {
	if c.op == "==" {
		return fmt.Sprintf("(%s == %s || %s == %s)", c.subject, quote(c.literal), c.subject, quote(shadowValue))
	}
	return fmt.Sprintf("(%s != %s && %s != %s)", c.subject, quote(c.literal), c.subject, quote(shadowValue))
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

// mentionsName reports whether s holds the repo name full as a whole name,
// in any case: not inside a longer owner or repo name such as
// acme/widget-docs. A / before it still counts (a URL or API path to the
// repo), and so does a .git after it.
func mentionsName(s, full string) bool {
	ls, lf := strings.ToLower(s), strings.ToLower(full)
	nameChar := func(i int) bool { return i < len(ls) && strings.IndexByte(nameChars, ls[i]) >= 0 }
	for i := 0; ; {
		j := strings.Index(ls[i:], lf)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(lf)
		if strings.HasPrefix(ls[end:], ".git") && !nameChar(end+4) {
			end += 4
		}
		if (start == 0 || !nameChar(start-1)) && !nameChar(end) {
			return true
		}
		i = start + 1
	}
}

// nameChars are the bytes, lowercased, that can continue an owner or repo
// name.
const nameChars = "abcdefghijklmnopqrstuvwxyz0123456789._-"

func removeFold(s, sub string) string {
	return regexp.MustCompile(`(?i)`+regexp.QuoteMeta(sub)).ReplaceAllString(s, "")
}

// guardExpr decides whether expr guards on the public repo. guard is false
// when it doesn't; rewritable is false when it does in a form the rewrite
// won't touch. out is expr with every guard comparison widened.
//
// A subject that expr already compares with the shadow value, with the same
// operator, is handled: its public comparisons are not guards, so a widened
// expression is never widened again.
func guardExpr(expr string, n Names) (out string, guard, rewritable bool) {
	pubOwner, shOwner := owner(n.Public), owner(n.Shadow)
	ownersDiffer := !strings.EqualFold(pubOwner, shOwner)
	values := func(c cmp) (isOwner bool, pubVal, shVal string) {
		if strings.EqualFold(c.subject, "github.repository_owner") {
			return true, pubOwner, shOwner
		}
		return false, n.Public, n.Shadow
	}
	key := func(c cmp) string { return strings.ToLower(c.subject) + " " + c.op }
	cs := comparisons(expr)
	handled := map[string]bool{}
	for _, c := range cs {
		if _, _, shVal := values(c); strings.EqualFold(c.literal, shVal) {
			handled[key(c)] = true
		}
	}
	var hits []cmp // in descending start order
	rem := expr
	for i := len(cs) - 1; i >= 0; i-- {
		c := cs[i]
		isOwner, pubVal, shVal := values(c)
		switch {
		case strings.EqualFold(c.literal, pubVal) && (!isOwner || ownersDiffer):
			if !handled[key(c)] {
				hits = append(hits, c)
			}
			rem = rem[:c.start] + rem[c.end:]
		case strings.EqualFold(c.literal, shVal) || (isOwner && strings.EqualFold(c.literal, pubOwner)):
			// already matches the shadow repo
			rem = rem[:c.start] + rem[c.end:]
		}
	}
	rem = removeFold(rem, n.Shadow)
	unrewritable := mentionsName(rem, n.Public) ||
		(ownersDiffer && containsFold(rem, "'"+pubOwner) && containsFold(rem, "repository"))
	switch {
	case unrewritable:
		return expr, true, false
	case len(hits) == 0:
		return expr, false, false
	}
	out = expr
	for _, c := range hits {
		sv := n.Shadow
		if strings.EqualFold(c.subject, "github.repository_owner") {
			sv = shOwner
		}
		out = out[:c.start] + widen(c, sv) + out[c.end:]
	}
	return out, true, true
}
