package server

import (
	"io/fs"
	"os/exec"
	"regexp"
	"testing"
)

// The script names its hints in the page's own tip, never in a native title (ui.md; nativetitle): a timestamp's zone, the
// error of a rule line and the time zone sample.
func TestScriptSetsNoNativeTitle(t *testing.T) {
	js, err := fs.ReadFile(webFS, "web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`\.title\s*\+?=[^=]|setAttribute\(\s*["']title["']|\btitle\s*[:,}]|["']title["']\s*[:\]]|createElement(NS)?\([^)]*["']title["']|\bel\(\s*["']title["']`)
	if m := re.FindAllIndex(js, -1); m != nil {
		for _, x := range m {
			t.Errorf("app.js sets a native title: %q", js[x[0]:x[1]])
		}
	}
}

func TestTimeScript(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	cmd := exec.Command(node, "--test", "time.test.js")
	cmd.Dir = "jstest"
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
