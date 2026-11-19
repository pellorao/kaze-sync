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
	t.Parallel()
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

// en keeps its own ".1" variants (the singular wording is the point of them).
func TestJSDictEnglishKeepsSingular(t *testing.T) {
	t.Parallel()
	en := jsDictMap(t, "en")
	n := 0
	for k, v := range i18nOf(t, "en") {
		if strings.HasSuffix(k, ".1") && strings.HasPrefix(k, "js.") {
			n++
			if en[k] != v {
				t.Errorf("en: %q is %q, want %q", k, en[k], v)
			}
		}
	}
	if n == 0 {
		t.Fatal("en has no js .1 keys: the test checks nothing")
	}
}

// A language that writes its own ".1" keeps it, and the English one of the same key does not replace it.
func TestJSDictKeepsOwnSingular(t *testing.T) {
	// Not parallel: it replaces the global ja dictionary and its script cache, which other tests read.
	dictMu.Lock()
	dicts["ja"] = map[string]string{"js.go": "ja {n}", "js.go.1": "ja one {n}"}
	dictMu.Unlock()
	t.Cleanup(func() { dictMu.Lock(); delete(dicts, "ja"); dictMu.Unlock(); jsDicts.Delete("ja") })
	jsDicts.Delete("ja")
	m := jsDictMap(t, "ja")
	if m["js.go"] != "ja {n}" || m["js.go.1"] != "ja one {n}" {
		t.Fatalf("ja: %q %q", m["js.go"], m["js.go.1"])
	}
}

func TestRtPlaceholdersReplacedOnce(t *testing.T) {
	t.Parallel()
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
