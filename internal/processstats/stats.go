package processstats

type Stats struct {
	CPUMillis     *uint64 `json:"cpuMillis"`
	ResidentBytes *uint64 `json:"residentBytes"`
}
