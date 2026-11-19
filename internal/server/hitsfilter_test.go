package server

import (
	"os/exec"
	"testing"
)

// The filter box above the hits of a rule preview must survive a refresh (web/app.js): typing while an answer is on
// its way used to lose characters.
func TestHitsFilterScript(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	cmd := exec.Command(node, "--test", "hitsfilter.test.js")
	cmd.Dir = "jstest"
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
