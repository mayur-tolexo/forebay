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
	"strconv"
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

// rfc is one RFC's header, parsed once and shared by every check that needs
// it, so there is a single answer to "what does this document say it is".
type rfc struct {
	number string
	file   string
	status string
	deps   []string
	body   string
}

// loadRFCs reads every numbered RFC and parses its header.
//
// A document whose header does not parse is a failure rather than a skip. The
// distinction is the whole point: an extra space in a table row renders
// identically in Markdown, and when parsing was lenient it silently dropped a
// document from every check here while both of them still passed. A check that
// quietly stops checking is worse than no check, because it reports success.
func loadRFCs(t *testing.T) []rfc {
	t.Helper()
	dir := filepath.Join(repoRoot(t), "docs", "rfcs")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	numbered := regexp.MustCompile(`^(\d{4})-.*\.md$`)
	var out []rfc
	for _, e := range entries {
		m := numbered.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		r := rfc{number: m[1], file: e.Name(), body: string(body)}
		r.status = headerField(r.body, "Status")
		if r.status == "" {
			t.Errorf("%s: no Status could be read from the header, so every check here would skip it", e.Name())
			continue
		}
		for _, d := range strings.Split(headerField(r.body, "Depends on"), ",") {
			if d = strings.TrimSpace(d); d != "" && d != "—" {
				r.deps = append(r.deps, d)
			}
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		t.Fatal("found no numbered RFCs, so nothing here proves anything")
	}
	return out
}

// headerField reads a value out of an RFC's header table, such as Status or
// Depends on, and returns empty when the row is absent or malformed.
func headerField(src, name string) string {
	m := regexp.MustCompile(`\|\s*\*\*` + regexp.QuoteMeta(name) + `\*\*\s*\|([^|]*)\|`).FindStringSubmatch(src)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(strings.ReplaceAll(m[1], "*", ""))
}

var (
	bulletPattern = regexp.MustCompile(`(?ms)^- (.+?)(?:\n- |\n\n|\z)`)
	// Deliberately narrow. Every alternative here can only be an ownership
	// claim, never ordinary prose. An earlier version accepted the bare words
	// "this document", which matched a question that explicitly said nobody
	// owned it, in the check whose whole purpose is catching that.
	ownerPattern = regexp.MustCompile(`(?i)owned by|owns it\b|owns this\b|owns both\b|owns the \w|has to measure|no rfc owns|no other rfc owns`)
)

func TestAcceptedRFCsOwnEveryOpenQuestion(t *testing.T) {
	// RFC-0000 requires an open question in an accepted RFC to name an owner,
	// or to say why it has none.
	checked := 0
	for _, r := range loadRFCs(t) {
		if r.status != "Accepted" {
			continue
		}
		checked++
		parts := strings.Split(r.body, "## Open questions")
		for _, m := range bulletPattern.FindAllStringSubmatch(parts[len(parts)-1], -1) {
			one := strings.Join(strings.Fields(m[1]), " ")
			if !ownerPattern.MatchString(one) {
				t.Errorf("%s: open question names no owner: %.70s", r.file, one)
			}
		}
	}
	if checked == 0 {
		t.Fatal("found no accepted RFCs, so this test proves nothing")
	}
	t.Logf("checked %d accepted RFCs", checked)
}

var rfcCountPattern = regexp.MustCompile(`All (\d+) RFCs`)

func TestTheAdvertisedRFCCountIsTheRealOne(t *testing.T) {
	// A number written in prose does not move when a file is added, and the
	// front page saying 27 when there are 28 is the sort of thing nobody
	// notices and every reader can check.
	root := repoRoot(t)
	entries, err := os.ReadDir(filepath.Join(root, "docs", "rfcs"))
	if err != nil {
		t.Fatalf("reading docs/rfcs: %v", err)
	}
	numbered := regexp.MustCompile(`^\d{4}-.*\.md$`)
	want := 0
	for _, e := range entries {
		if numbered.MatchString(e.Name()) {
			want++
		}
	}
	if want == 0 {
		t.Fatal("found no numbered RFCs, so this test proves nothing")
	}

	body, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatalf("reading README.md: %v", err)
	}
	m := rfcCountPattern.FindStringSubmatch(string(body))
	if m == nil {
		t.Fatal(`README.md no longer says "All N RFCs", so this check has stopped checking anything`)
	}
	if m[1] != strconv.Itoa(want) {
		t.Errorf("README.md advertises %s RFCs, but there are %d", m[1], want)
	}
}

var indexRowPattern = regexp.MustCompile(`(?m)^\| \[(\d{4})\][^|]*\|([^|]*)\|([^|]*)\|`)

func TestEveryRFCAppearsInTheIndexAndEveryIndexRowExists(t *testing.T) {
	// The index is the page people navigate from, and a number in the README
	// does not notice a missing row. Both directions matter: an RFC absent
	// from the index is invisible, and a row pointing at nothing is a broken
	// promise that the link check cannot see, because the row's own link is
	// the thing that would be wrong.
	root := repoRoot(t)
	dir := filepath.Join(root, "docs", "rfcs")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	numbered := regexp.MustCompile(`^(\d{4})-.*\.md$`)
	files := map[string]bool{}
	for _, e := range entries {
		if m := numbered.FindStringSubmatch(e.Name()); m != nil {
			files[m[1]] = true
		}
	}
	if len(files) == 0 {
		t.Fatal("found no numbered RFCs, so this test proves nothing")
	}

	body, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		t.Fatalf("reading the index: %v", err)
	}
	rows := map[string]string{}
	for _, m := range indexRowPattern.FindAllStringSubmatch(string(body), -1) {
		rows[m[1]] = strings.TrimSpace(strings.ReplaceAll(m[3], "*", ""))
	}
	if len(rows) == 0 {
		t.Fatal("the index has no RFC rows, so this check has stopped checking anything")
	}

	for n := range files {
		if _, listed := rows[n]; !listed {
			t.Errorf("RFC %s exists but the index does not list it", n)
		}
	}
	for n := range rows {
		if !files[n] {
			t.Errorf("the index lists RFC %s, but no such file exists", n)
		}
	}

	// Presence is not agreement. Accepting an RFC means editing the document
	// and the index, which is two places to change and one to forget, and the
	// status is the field that actually moves.
	for _, r := range loadRFCs(t) {
		idx, listed := rows[r.number]
		if !listed {
			continue // Already reported above.
		}
		if idx != r.status {
			t.Errorf("RFC %s says it is %q but the index says %q", r.number, r.status, idx)
		}
	}
}

func TestAnAcceptedRFCDependsOnlyOnAcceptedRFCs(t *testing.T) {
	// Accepting a document whose foundation is still a draft, or unwritten,
	// makes the status mean less than it says: the reasoning underneath it can
	// still change. The corpus has always held this, and encoding it turns a
	// convention nobody wrote down into one that cannot be broken quietly.
	rfcs := loadRFCs(t)
	status := map[string]string{}
	for _, r := range rfcs {
		status[r.number] = r.status
	}

	accepted := 0
	for _, r := range rfcs {
		if r.status != "Accepted" {
			continue
		}
		accepted++
		for _, d := range r.deps {
			ds, known := status[d]
			switch {
			case !known:
				t.Errorf("RFC %s is Accepted and depends on %s, which does not exist", r.number, d)
			case ds != "Accepted":
				t.Errorf("RFC %s is Accepted but depends on %s, which is %s", r.number, d, ds)
			}
		}
	}
	if accepted == 0 {
		t.Fatal("found no accepted RFCs, so this test proves nothing")
	}
	t.Logf("checked %d accepted RFCs", accepted)
}
