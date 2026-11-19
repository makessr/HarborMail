package main

import (
	"fmt"
	"io"
	"os"
	"strings"
)

const settingFileLimit = 1 << 20

func fileSetting(key string) (string, error) {
	inEnv := os.Getenv(key)
	path := os.Getenv(key + "_FILE")
	if path == "" {
		return inEnv, nil
	}
	if inEnv != "" {
		return "", fmt.Errorf("set only one of %s and %s_FILE", key, key)
	}
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("could not open the file configured by %s_FILE: %w", key, err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, settingFileLimit+1))
	if err != nil {
		return "", fmt.Errorf("could not read the file configured by %s_FILE: %w", key, err)
	}
	if len(data) > settingFileLimit {
		return "", fmt.Errorf("the file configured by %s_FILE exceeds 1 MiB", key)
	}
	return strings.TrimRight(string(data), "\r\n"), nil
}
