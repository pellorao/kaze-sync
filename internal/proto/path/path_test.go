package path

import "testing"

func TestCheckPortable(t *testing.T) {
	bad := []string{
		"con", "CON.txt", "Com1.tar.gz", "lpt9", "dir/nul", "AUX", "prn.x/ok", "COM9",
		"a:b.txt", "a<b", "a>b", `a"b`, `a\b`, "a|b", "a?b", "a*b", "a\x00b", "a\x1fb",
		"dir.", "dir /x", "x/y ", "a//b", "/a", "a/", "", "bad\xffutf8", "..", "a/./b",
	}
	for _, p := range bad {
		if err := CheckPortable(p); err == nil {
			t.Errorf("CheckPortable(%q) accepted", p)
		}
	}
	good := []string{"COM0", "CONSOLE", "console.txt", "LPT10", "a.b", "文档/报告.docx", "x y/z", ".syncroot", "~$tmp", "a-b_c (1).txt", "NULL", "comx"}
	for _, p := range good {
		if err := CheckPortable(p); err != nil {
			t.Errorf("CheckPortable(%q): %v", p, err)
		}
	}
}

func TestKey(t *testing.T) {
	nfc, nfd := "café", "café"
	if Key(nfc) != Key(nfd) {
		t.Errorf("NFC and NFD keys differ: %q %q", Key(nfc), Key(nfd))
	}
	if Key("Docs/Report.TXT") != Key("docs/report.txt") {
		t.Error("ASCII case differs")
	}
	if Key("É") != Key("é") {
		t.Error("case plus normalization differs")
	}
	if Key("\u212a") != Key("k") { // Kelvin sign
		t.Error("Kelvin sign does not fold to k")
	}
	// Simple (not full) case folding: sharp s stays itself and does not equal "ss".
	if Key("Straße") == Key("STRASSE") {
		t.Error("Straße must not equal STRASSE under simple folding")
	}
	if Key("Straße") != Key("STRAßE") || Key("ẞ") != Key("ß") {
		t.Error("simple folding of ß/ẞ changed")
	}
	if Key("a") == Key("b") {
		t.Error("distinct names share a key")
	}
}
