package processstats

import (
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func Read() Stats {
	var result Stats
	var usage syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &usage) == nil {
		millis := uint64((usage.Utime.Nano() + usage.Stime.Nano()) / int64(time.Millisecond))
		result.CPUMillis = &millis
	}
	if data, err := os.ReadFile("/proc/self/statm"); err == nil {
		fields := strings.Fields(string(data))
		if len(fields) >= 2 {
			if pages, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
				bytes := pages * uint64(os.Getpagesize())
				result.ResidentBytes = &bytes
			}
		}
	}
	return result
}
