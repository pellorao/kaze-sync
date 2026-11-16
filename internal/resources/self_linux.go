package resources

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

func ReadSelf() (Reading, error) {
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return Reading{}, err
	}
	stat, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return Reading{}, err
	}
	return parseLinux(string(status), string(stat))
}
func parseLinux(status, stat string) (Reading, error) {
	var r Reading
	for line := range strings.SplitSeq(status, "\n") {
		f := strings.Fields(line)
		if len(f) != 3 || (f[0] != "VmRSS:" && f[0] != "VmHWM:") {
			continue
		}
		n, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil || f[2] != "kB" || n > ^uint64(0)/1024 {
			return r, fmt.Errorf("invalid process memory")
		}
		if f[0] == "VmRSS:" {
			r.RSS = n * 1024
		} else {
			r.HWM = n * 1024
		}
	}
	// comm may contain spaces and closing parentheses. Fields after its final ')'
	// start at field 3 (state); utime and stime are fields 14 and 15.
	i := strings.LastIndexByte(stat, ')')
	if i < 0 {
		return r, fmt.Errorf("invalid process stat")
	}
	f := strings.Fields(stat[i+1:])
	if len(f) < 13 || r.RSS == 0 {
		return r, fmt.Errorf("incomplete process reading")
	}
	u, err := strconv.ParseUint(f[11], 10, 64)
	if err != nil {
		return r, err
	}
	s, err := strconv.ParseUint(f[12], 10, 64)
	if err != nil {
		return r, err
	}
	// Linux exports /proc process CPU in USER_HZ units (100), independent of CONFIG_HZ.
	r.CPU = float64(u)/100 + float64(s)/100
	return r, nil
}
