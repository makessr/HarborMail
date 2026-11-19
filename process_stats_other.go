//go:build !windows && !linux

package main

func readProcessStats() processStats {
	return processStats{}
}
