package resources

import (
	"strings"
	"testing"
)

func TestLinuxProcessParsing(t *testing.T) {
	fields := []string{"S", "1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "125", "75"}
	stat := "123 (a name ) with parentheses) " + strings.Join(fields, " ")
	r, err := parseLinux("VmRSS:\t123 kB\nVmHWM:\t456 kB\n", stat)
	if err != nil || r.RSS != 123*1024 || r.HWM != 456*1024 || r.CPU != 2 {
		t.Fatalf("%+v %v", r, err)
	}
	for _, tc := range []struct{ status, stat string }{
		{"VmRSS: 1 MB", stat}, {"VmRSS: x kB", stat}, {"VmRSS: 1 kB", "123 (bad) S"}, {"VmRSS: 1 kB", strings.Replace(stat, "125", "bad", 1)},
	} {
		if _, err := parseLinux(tc.status, tc.stat); err == nil {
			t.Fatalf("accepted malformed %+v", tc)
		}
	}
}
