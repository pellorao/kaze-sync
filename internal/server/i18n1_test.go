package server

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// i18n1: the ".1" variant (the text for the count 1) of a key is served only from the language that owns the key's text;
// an English-only ".1" must not be merged into another language, where rt() would show it in English.

func jsDictMap(t *testing.T, code string) map[string]string {
	t.Helper()
	script, _ := jsDict(code)
	js := strings.TrimSuffix(strings.TrimPrefix(script, "window.KS_I18N="), ";")
	m := map[string]string{}
	if err := json.Unmarshal([]byte(js), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestJSDictNoForeignSingular(t *testing.T) {
	for _, l := range uiLangs {
		own := i18nOf(t, l)
		for k := range jsDictMap(t, l) {
			if !strings.HasSuffix(k, ".1") {
				continue
			}
			if _, ok := own[k]; !ok && l != "en" {
				t.Errorf("%s: jsDict carries %q, which the language does not define (it would show English at count 1)", l, k)
			}
		}
	}
}

func TestRtPlaceholdersReplacedOnce(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	cmd := exec.Command(node, "--test", "rt.test.js")
	cmd.Dir = "jstest"
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

