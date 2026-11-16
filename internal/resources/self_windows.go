package resources

import (
	"golang.org/x/sys/windows"
	"unsafe"
)

var getProcessMemoryInfo = windows.NewLazySystemDLL("psapi.dll").NewProc("GetProcessMemoryInfo")

// PROCESS_MEMORY_COUNTERS uses pointer-sized SIZE_T values after its two DWORDs.
type memoryCounters struct {
	Size, Faults                                                                                 uint32
	PeakWorkingSet, WorkingSet, PeakPaged, Paged, PeakNonPaged, NonPaged, Pagefile, PeakPagefile uintptr
}

func ReadSelf() (Reading, error) {
	h := windows.CurrentProcess()
	m := memoryCounters{}
	m.Size = uint32(unsafe.Sizeof(m))
	ok, _, err := getProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&m)), uintptr(m.Size))
	if ok == 0 {
		return Reading{}, err
	}
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return Reading{}, err
	}
	seconds := func(f windows.Filetime) float64 {
		return float64(uint64(f.HighDateTime)<<32|uint64(f.LowDateTime)) / 1e7
	}
	return Reading{RSS: uint64(m.WorkingSet), HWM: uint64(m.PeakWorkingSet), CPU: seconds(kernel) + seconds(user)}, nil
}
