package main

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var getProcessMemoryInfo = windows.NewLazySystemDLL("psapi.dll").NewProc("GetProcessMemoryInfo")

// See PROCESS_MEMORY_COUNTERS in psapi.h. SIZE_T is pointer-sized.
type processMemoryCounters struct {
	Size, PageFaults               uint32
	PeakWorkingSet, WorkingSet     uintptr
	PeakPagedPool, PagedPool       uintptr
	PeakNonPagedPool, NonPagedPool uintptr
	Pagefile, PeakPagefile         uintptr
}

func readProcessStats() processStats {
	var result processStats
	handle := windows.CurrentProcess()
	var created, exited, kernel, user windows.Filetime
	if windows.GetProcessTimes(handle, &created, &exited, &kernel, &user) == nil {
		ticks := func(value windows.Filetime) uint64 {
			return uint64(value.HighDateTime)<<32 | uint64(value.LowDateTime)
		}
		millis := (ticks(kernel) + ticks(user)) / 10000
		result.CPUMillis = &millis
	}
	var counters processMemoryCounters
	counters.Size = uint32(unsafe.Sizeof(counters))
	if getProcessMemoryInfo.Find() == nil {
		ok, _, _ := getProcessMemoryInfo.Call(uintptr(handle), uintptr(unsafe.Pointer(&counters)), uintptr(counters.Size))
		if ok != 0 {
			bytes := uint64(counters.WorkingSet)
			result.ResidentBytes = &bytes
		}
	}
	return result
}
