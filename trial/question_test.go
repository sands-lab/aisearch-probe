package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"aisearch-probe/internal/probe"
)

// THE QUESTION RULE (DECISIONS.md § D14, the user, 2026-10-25). Every runner asks
// standardQuestion(depth). Any other wording needs the user's approval FIRST, with its
// reason, recorded in DECISIONS.md — never just a code comment. This registry lists
// every question declared in the runners (any const, var, func or method whose name
// contains "question") with its status:
//
//	standard   asks standardQuestion (checked word for word below)
//	exception  a different wording the user approved, with the reason, in D14
//	legacy     an old test the user approved keeping as is (D14); new runs never use it
//	selector   only picks among the questions above; holds no wording of its own
//
// A new declaration that is not listed fails TestEveryQuestionIsRegistered. Adding it
// here is the step that must wait for the user's approval and a DECISIONS.md entry.
var questionRegistry = map[string]string{
	// standard
	"trial standardQuestion":        "standard",
	"trial sharingAskQuestion":      "standard",
	"trial sharingWarmQuestion":     "standard",
	"trial healWarmQuestion":        "standard",
	"trial healReaskQuestion":       "standard",
	"trial correctionWarmQuestion":  "standard",
	"trial correctionReaskQuestion": "standard",
	"trial rotatedQuestion":         "standard",
	// approved exceptions (D14): the question is itself the measurement, or the page
	// is not ours / has no depth codes
	"trial truncScanQuestion": "exception", // read-depth part A: every code, to see how far down it read
	"trial faultQuestion":     "exception", // fault test: what the page says + every code
	// legacy (D14): an older wording kept for pages without depth codes
	"trial ttlmapQuestion": "legacy",
}

// declaredQuestions parses a package directory (non-test files) and returns every
// declaration whose name contains "question", as "<pkg> <name>" or, for a method,
// "<pkg> <Receiver>.<name>".
func declaredQuestions(t *testing.T, dir, pkg string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		for _, d := range af.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if !strings.Contains(strings.ToLower(d.Name.Name), "question") {
					continue
				}
				name := d.Name.Name
				if d.Recv != nil && len(d.Recv.List) > 0 {
					switch rt := d.Recv.List[0].Type.(type) {
					case *ast.Ident:
						name = rt.Name + "." + name
					case *ast.StarExpr:
						if id, ok := rt.X.(*ast.Ident); ok {
							name = id.Name + "." + name
						}
					}
				}
				out = append(out, pkg+" "+name)
			case *ast.GenDecl:
				for _, s := range d.Specs {
					if vs, ok := s.(*ast.ValueSpec); ok {
						for _, id := range vs.Names {
							if strings.Contains(strings.ToLower(id.Name), "question") {
								out = append(out, pkg+" "+id.Name)
							}
						}
					}
				}
			}
		}
	}
	return out
}

func TestEveryQuestionIsRegistered(t *testing.T) {
	found := declaredQuestions(t, ".", "trial")
	seen := map[string]bool{}
	for _, name := range found {
		seen[name] = true
		if _, ok := questionRegistry[name]; !ok {
			t.Errorf("%s is a question not in the registry. A wording other than standardQuestion needs the user's approval and a DECISIONS.md entry (§ D14) before it is listed here", name)
		}
	}
	var stale []string
	for name := range questionRegistry {
		if !seen[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	for _, name := range stale {
		t.Errorf("registry lists %s, which no longer exists — remove it", name)
	}
}

// Everything registered "standard" really asks the standard wording.
func TestStandardQuestionsAreStandard(t *testing.T) {
	deepest := probe.DepthPercents[len(probe.DepthPercents)-1]
	for _, d := range probe.DepthPercents {
		if sharingAskQuestion(d) != standardQuestion(d) || rotatedQuestion(d) != standardQuestion(d) {
			t.Errorf("depth %d: sharing/rotated question differs from standardQuestion", d)
		}
	}
	for name, got := range map[string][2]string{
		"sharingWarmQuestion":     {sharingWarmQuestion, standardQuestion(deepest)},
		"healWarmQuestion":        {healWarmQuestion, standardQuestion(deepest)},
		"healReaskQuestion":       {healReaskQuestion, standardQuestion(50)},
		"correctionWarmQuestion":  {correctionWarmQuestion, standardQuestion(deepest)},
		"correctionReaskQuestion": {correctionReaskQuestion, standardQuestion(50)},
	} {
		if got[0] != got[1] {
			t.Errorf("%s = %q, want the standard question %q", name, got[0], got[1])
		}
	}
	// The standard question points at a code by the words the depth page prints next
	// to it ("…for the section at depth=N%"); proxy's TestCcDepthPageCarriesEveryNamedCode
	// pins those words to the served page through probe.DepthMarkerLabel.
	for _, d := range probe.DepthPercents {
		q := standardQuestion(d)
		phrase := q[strings.Index(q, "for the section"):strings.Index(q, " in it.")] // "for the section at depth=N%"
		if !strings.HasSuffix(probe.DepthMarkerLabel(d), phrase) {
			t.Errorf("depth %d: the question's phrase %q is not how the page labels the code (%q)", d, phrase, probe.DepthMarkerLabel(d))
		}
	}
}
