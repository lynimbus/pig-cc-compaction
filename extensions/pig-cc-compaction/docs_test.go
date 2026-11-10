package pigcc

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// cgoDisabledEnv is the one documented string with no production-code source:
// this extension needs no cgo, and the prerequisite is an operator action on
// the machine (go env -w), not a branch in Go code. The literal lives here so
// the documentation contract still names the exact setting; TestDocsMatchCode
// checks that README.md and docs/verify.md spell it the same way.
const cgoDisabledEnv = "CGO_ENABLED=0"

// documentedStrings must appear in both README.md and docs/verify.md. Four of
// them also have a production source in extension.go or test.sh; see
// TestDocsMatchCode.
var documentedStrings = []string{
	"PIG_CC_PLUGIN",
	"PIG_CC_TRACE",
	"cc-compact",
	"pi-cc-trace.jsonl",
	cgoDisabledEnv,
}

// readDoc reads a repository file and reports the path on failure, so a broken
// doc test always names the file whose contract drifted.
func readDoc(t *testing.T, name string) string {
	t.Helper()

	body, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(body)
}

// missingDocStrings is the pure half of the documentation check: it returns one
// "<file>: missing "<string>"" line per absent contract string. Tests use it
// directly so a temp-dir copy can prove a deletion is caught without touching
// the real README.
func missingDocStrings(name, body string, want []string) []string {
	var missing []string
	for _, needle := range want {
		if !strings.Contains(body, needle) {
			missing = append(missing, fmt.Sprintf("%s: missing %q", name, needle))
		}
	}
	return missing
}

// sourceCorpus is the production side of the doc contract: every root Go file
// except tests, plus test.sh, because either may be where a documented name is
// defined.
func sourceCorpus(t *testing.T) map[string]string {
	t.Helper()

	corpus := make(map[string]string)

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob *.go: %v", err)
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		corpus[name] = readDoc(t, name)
	}

	corpus["test.sh"] = readDoc(t, "test.sh")
	return corpus
}

// sourceFiles returns the corpus file names that contain needle, so a failure
// can say what was scanned rather than only what was missing.
func sourceFiles(corpus map[string]string, needle string) []string {
	var found []string
	for name, body := range corpus {
		if strings.Contains(body, needle) {
			found = append(found, name)
		}
	}
	return found
}

func corpusNames(corpus map[string]string) string {
	names := make([]string, 0, len(corpus))
	for name := range corpus {
		names = append(names, name)
	}
	return strings.Join(names, ", ")
}

// TestDocsMatchCode keeps README.md and docs/verify.md from drifting away from
// the code they describe. It checks both directions:
//
//   - the five documented contract strings are present in both docs;
//   - four of them have a production source, and the fifth is the explicit
//     machine prerequisite anchored by cgoDisabledEnv;
//   - extension.go's pluginEnv and traceEnv values, extracted from the source
//     rather than duplicated here, appear verbatim in both docs.
//
// The docs live at the package root, one level above this Go module, which
// PiG's package layout puts under extensions/.
const (
	readmePath = "../../README.md"
	guidePath  = "../../docs/verify.md"
)

func TestDocsMatchCode(t *testing.T) {
	readme := readDoc(t, readmePath)
	guide := readDoc(t, guidePath)

	docs := []struct {
		name string
		body string
	}{
		{"README.md", readme},
		{guidePath, guide},
	}
	for _, doc := range docs {
		if missing := missingDocStrings(doc.name, doc.body, documentedStrings); len(missing) > 0 {
			t.Fatalf("documentation is missing contract strings:\n%s", strings.Join(missing, "\n"))
		}
	}

	corpus := sourceCorpus(t)
	for _, needle := range documentedStrings {
		if len(sourceFiles(corpus, needle)) > 0 {
			continue
		}

		// CGO_ENABLED=0 has no production literal: no Go file needs cgo, and
		// test.sh only invokes the Go toolchain. The checked source is this test's
		// own cgoDisabledEnv constant, plus README's exact "go env -w" line and
		// env file path, which TestDocsMatchCode asserts below.
		if needle == cgoDisabledEnv {
			if !strings.Contains(readDoc(t, "docs_test.go"), needle) {
				t.Fatalf("docs_test.go: source-side anchor %q is missing", needle)
			}
			if !strings.Contains(readme, "go env -w "+cgoDisabledEnv) {
				t.Fatalf("README.md: must show %q as the exact go env -w assignment", "go env -w "+cgoDisabledEnv)
			}
			if !strings.Contains(readme, "~/.config/go/env") {
				t.Fatalf("README.md: must name ~/.config/go/env as where %s is written", cgoDisabledEnv)
			}
			continue
		}

		t.Fatalf("source side: %q is documented but absent from production sources (%s)", needle, corpusNames(corpus))
	}

	// Reverse direction: read the constants out of extension.go instead of
	// spelling the names a second time here, so a rename in the Go source
	// fails this test until the docs follow.
	constRe := regexp.MustCompile(`(?m)^\s*(pluginEnv|traceEnv)\s*=\s*"([^"]+)"`)
	values := make(map[string]string)
	for _, match := range constRe.FindAllStringSubmatch(readDoc(t, "extension.go"), -1) {
		values[match[1]] = match[2]
	}
	for _, name := range []string{"pluginEnv", "traceEnv"} {
		value := values[name]
		if value == "" {
			t.Fatalf("extension.go: cannot extract the %s string literal", name)
		}
		for _, doc := range docs {
			if !strings.Contains(doc.body, value) {
				t.Fatalf("%s: does not document %s = %q from extension.go", doc.name, name, value)
			}
		}
	}

	// The temp-dir proof that the checker actually fails on a deletion: remove
	// PIG_CC_TRACE from a copy of README.md and require the diagnostic to name
	// both the string and the file. The real README stays untouched.
	t.Run("removed README string is reported with file and value", func(t *testing.T) {
		broken := strings.ReplaceAll(readme, "PIG_CC_TRACE", "")
		if broken == readme {
			t.Fatalf("README.md: %q not found, cannot build the missing-string case", "PIG_CC_TRACE")
		}

		copyPath := filepath.Join(t.TempDir(), "README.md")
		if err := os.WriteFile(copyPath, []byte(broken), 0o644); err != nil {
			t.Fatalf("write %s: %v", copyPath, err)
		}
		body, err := os.ReadFile(copyPath)
		if err != nil {
			t.Fatalf("read %s: %v", copyPath, err)
		}

		missing := missingDocStrings("README.md", string(body), documentedStrings)
		if len(missing) == 0 {
			t.Fatalf("removing %q from README.md produced no missing-string diagnostic", "PIG_CC_TRACE")
		}
		report := strings.Join(missing, "\n")
		for _, want := range []string{"PIG_CC_TRACE", "README.md"} {
			if !strings.Contains(report, want) {
				t.Fatalf("diagnostic does not mention %q:\n%s", want, report)
			}
		}
	})
}

// TestVerifyGuideCoversAssertions pins the pieces a reader needs in the
// end-to-end guide: the RPC invocation, the compact command, the extension
// event field, the cache-read metric, and the troubleshooting terms.
func TestVerifyGuideCoversAssertions(t *testing.T) {
	guide := readDoc(t, guidePath)

	required := []string{
		"--mode rpc",
		`"type":"compact"`,
		"fromExtension",
		"cacheRead",
	}
	if missing := missingDocStrings("docs/verify.md", guide, required); len(missing) > 0 {
		t.Fatalf("verification guide is missing required fragments:\n%s", strings.Join(missing, "\n"))
	}

	troubleshooting := []string{
		"session_compact",
		"fallback",
		"stderrLog",
		"name_mismatch",
		"updates to go.mod needed",
	}
	if missing := missingDocStrings("docs/verify.md", guide, troubleshooting); len(missing) > 0 {
		t.Fatalf("verification guide is missing troubleshooting material:\n%s", strings.Join(missing, "\n"))
	}
}
