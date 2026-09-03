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

	// Deliberately narrow. Every alternative here can only be an ownership
	// claim, never ordinary prose. An earlier version accepted the bare words
	// "this document", which matched a question that explicitly said nobody
	// owned it, in the check whose whole purpose is catching that.
	ownerPattern = regexp.MustCompile(`(?i)owned by|owns it\b|owns this\b|owns both\b|owns the \w|has to measure|no rfc owns|no other rfc owns`)
)

// openQuestions returns every bullet under the open questions heading, one
// string each with its wrapping collapsed.
//
// Scanned by line rather than matched. A pattern that stops at the "\n- "
// starting the next bullet also consumes it, which leaves the scan past that
// bullet's own start and skips it: half the questions went unexamined, and
// unnoticed because the skipped half happened to be the ones that would have
// passed. Splitting on the same separator has the opposite fault, taking
// bullets from whatever section comes next, so the list ends where a blank
// line ends it.
func openQuestions(body string) []string {
	parts := strings.Split(body, "## Open questions")
	if len(parts) < 2 {
		return nil
	}
	var out, cur []string
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.Join(cur, " "))
			cur = nil
		}
	}
	for _, line := range strings.Split(parts[len(parts)-1], "\n") {
		switch {
		case strings.HasPrefix(line, "- "):
			flush()
			cur = append(cur, strings.TrimSpace(strings.TrimPrefix(line, "- ")))
		case strings.TrimSpace(line) == "":
			flush()
			// The blank line before the first bullet is not the end of a list
			// that has not started.
			if len(out) > 0 {
				return out
			}
		case len(cur) > 0:
			cur = append(cur, strings.TrimSpace(line))
		}
	}
	flush()
	return out
}

// TestOpenQuestionsFindsEveryBullet covers the reader rather than the rule.
// The pattern it replaced examined every second question, so a document could
// fail this check and pass, which is worse than not checking at all.
func TestOpenQuestionsFindsEveryBullet(t *testing.T) {
	body := "## Open questions\n\n" +
		"- first, which wraps\n  onto another line\n" +
		"- second\n" +
		"- third\n" +
		"- fourth\n\n" +
		"## Something else\n\n- not a question\n"

	got := openQuestions(body)
	if len(got) != 4 {
		t.Fatalf("found %d questions, want 4: %q", len(got), got)
	}
	if got[0] != "first, which wraps onto another line" {
		t.Errorf("first = %q, want its wrapping collapsed", got[0])
	}
	if got[3] != "fourth" {
		t.Errorf("fourth = %q", got[3])
	}
	for _, q := range got {
		if strings.Contains(q, "not a question") {
			t.Errorf("a bullet from the next section was taken: %q", q)
		}
	}
	if n := len(openQuestions("no heading here")); n != 0 {
		t.Errorf("a document with no open questions gave %d", n)
	}
}

func TestAcceptedRFCsOwnEveryOpenQuestion(t *testing.T) {
	// RFC-0000 requires an open question in an accepted RFC to name an owner,
	// or to say why it has none.
	checked := 0
	for _, r := range loadRFCs(t) {
		if r.status != "Accepted" {
			continue
		}
		checked++
		for _, one := range openQuestions(r.body) {
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

// Both places the README states a count: the prose link and the badge. The
// badge was missed the first time, and a number nothing checks is a number
// that goes stale.
var rfcCountPatterns = []*regexp.Regexp{
	regexp.MustCompile(`All (\d+) RFCs`),
	regexp.MustCompile(`RFCs-(\d+)%20open`),
	regexp.MustCompile(`(\d+) RFCs open`),
}

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

	// Every file that states the count, including the social card, which is an
	// image but is also text and rots like any other number.
	for _, f := range []string{"README.md", filepath.Join("docs", "brand", "social-card.svg")} {
		body, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Fatalf("reading %s: %v", f, err)
		}
		matched := false
		for _, pattern := range rfcCountPatterns {
			m := pattern.FindStringSubmatch(string(body))
			if m == nil {
				continue
			}
			matched = true
			if m[1] != strconv.Itoa(want) {
				t.Errorf("%s advertises %s RFCs, but there are %d", f, m[1], want)
			}
		}
		if !matched {
			t.Errorf("%s no longer states an RFC count, so this check has stopped checking it", f)
		}
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
