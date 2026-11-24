package server

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// A device that never reported shows "未上报" in the overview's folder-status cell (design 4.x, overview).
func TestOverviewUnreportedDeviceCell(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.setAdminPassword("pw")
	c := e.webClient()
	e.login(c, "pw")
	e.enrollDevice("A", "linux")
	_, body := e.get(c, "/")
	if !regexp.MustCompile(`<td class="top"><span class="muted">未上报</span></td>`).MatchString(body) {
		t.Errorf("the status cell of a device that never reported is not the unreported word:\n%s", body)
	}
}

// The settings page asks before the bell panel's links and the logout form drop unsaved rules (design: the same
// question as for the header links).
func TestSettingsLeaveGuardCoversPanelAndLogout(t *testing.T) {
	t.Parallel()
	js, _ := fs.ReadFile(webFS, "web/app.js")
	for _, want := range []string{`closest("#notif a[href]")`, `$('form[action="/logout"]')`, `addEventListener("submit"`} {
		if !strings.Contains(string(js), want) {
			t.Errorf("app.js lacks %s", want)
		}
	}
}
