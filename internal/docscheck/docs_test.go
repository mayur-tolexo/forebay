// Package docscheck holds invariants about the documentation that are cheaper
// to enforce than to remember.
//
// They are tests rather than a script so that `make check` and CI run them
// without anyone choosing to, since a check that has to be invoked is a check
// that stops being invoked.
package docscheck

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// repoRoot is this package's directory less internal/docscheck.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Dir(filepath.Dir(wd))
}

// markdownFiles lists every tracked markdown file.
func markdownFiles(t *testing.T) []string {
	t.Helper()
	root := repoRoot(t)
	var out []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "bin", ".worktrees":
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(p, ".md") {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	if len(out) == 0 {
		t.Fatalf("found no markdown under %s, so this test proves nothing", root)
	}
	return out
}

// rel shortens a path for failure messages.
func rel(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.Rel(repoRoot(t), p)
	if err != nil {
		return p
	}
	return r
}

func TestNoBlankLineInsideATable(t *testing.T) {
	// GitHub ends a table at the first blank line, so rows after one render as
	// literal pipe-delimited text. This has happened twice.
	for _, p := range markdownFiles(t) {
		body, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("reading %s: %v", p, err)
		}
		lines := strings.Split(string(body), "\n")
		for i := 1; i < len(lines)-1; i++ {
			if strings.TrimSpace(lines[i]) == "" &&
				strings.HasPrefix(lines[i-1], "|") && strings.HasPrefix(lines[i+1], "|") {
				t.Errorf("%s:%d: blank line inside a table truncates it", rel(t, p), i+1)
			}
		}
	}
}

var linkPattern = regexp.MustCompile(`\]\(([^)#]+?)(#[^)]*)?\)`)

func TestEveryRelativeLinkResolves(t *testing.T) {
	for _, p := range markdownFiles(t) {
		body, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("reading %s: %v", p, err)
		}
		for _, m := range linkPattern.FindAllStringSubmatch(string(body), -1) {
			target := strings.TrimSpace(m[1])
			switch {
			case strings.HasPrefix(target, "http"), strings.HasPrefix(target, "mailto:"),
				strings.HasPrefix(target, "../../issues"):
				continue
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(p), target)); err != nil {
				t.Errorf("%s: link to %q does not resolve", rel(t, p), target)
			}
		}
	}
}

var (
	acceptedMarker = "| **Status** | Accepted |"
	bulletPattern  = regexp.MustCompile(`(?ms)^- (.+?)(?:\n- |\n\n|\z)`)
	// Deliberately narrow. Every alternative here can only be an ownership
	// claim, never ordinary prose. An earlier version accepted the bare words
	// "this document", which matched a question that explicitly said nobody
	// owned it, in the check whose whole purpose is catching that.
	ownerPattern = regexp.MustCompile(`(?i)owned by|owns it\b|owns this\b|owns both\b|owns the \w|has to measure|no rfc owns|no other rfc owns`)
)

func TestAcceptedRFCsOwnEveryOpenQuestion(t *testing.T) {
	// RFC-0000 requires an open question in an accepted RFC to name an owner,
	// or to say why it has none.
	dir := filepath.Join(repoRoot(t), "docs", "rfcs")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	checked := 0
	for _, e := range entries {
		if !regexp.MustCompile(`^\d{4}-`).MatchString(e.Name()) {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		src := string(body)
		if !strings.Contains(src, acceptedMarker) {
			continue
		}
		checked++
		parts := strings.Split(src, "## Open questions")
		for _, m := range bulletPattern.FindAllStringSubmatch(parts[len(parts)-1], -1) {
			one := strings.Join(strings.Fields(m[1]), " ")
			if !ownerPattern.MatchString(one) {
				t.Errorf("%s: open question names no owner: %.70s", e.Name(), one)
			}
		}
	}
	if checked == 0 {
		t.Fatal("found no accepted RFCs, so this test proves nothing")
	}
	t.Logf("checked %d accepted RFCs", checked)
}
