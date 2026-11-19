//go:build !windows && !linux

package processstats

func Read() Stats {
	return Stats{}
}
