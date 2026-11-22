package server

import (
	"cmp"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Release builds and the Dockerfile set these with the Go linker.
var (
	buildVersion    = "dev"
	buildDeployment = "binary"
)

func deploymentDetails(goos, goarch, kind string) (label, asset string) {
	platform := goos
	switch goos {
	case "linux":
		platform = "Linux"
	case "windows":
		platform = "Windows"
	}
	architecture := strings.ToUpper(goarch)
	if kind == "docker" {
		return "Docker · " + platform + " " + architecture, ""
	}
	label = platform + " " + architecture + " · 二进制"
	if (goos == "linux" || goos == "windows") && (goarch == "amd64" || goarch == "arm64") {
		asset = "harbormail-" + goos + "-" + goarch
		if goos == "windows" {
			asset += ".exe"
		}
	}
	return label, asset
}

var stableVersionPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

func stableVersion(tag string) ([3]uint64, bool) {
	var version [3]uint64
	if !stableVersionPattern.MatchString(tag) {
		return version, false
	}
	for i, part := range strings.Split(tag[1:], ".") {
		number, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return version, false
		}
		version[i] = number
	}
	return version, true
}

func currentBuildVersion() string {
	if _, ok := stableVersion(buildVersion); ok {
		return buildVersion
	}
	return "dev"
}

const latestReleaseAPI = "https://api.github.com/repos/makessr/HarborMail/releases/latest"

type stableRelease struct {
	Tag        string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Assets     []struct {
		Name  string `json:"name"`
		State string `json:"state"`
	} `json:"assets"`
}

func (release stableRelease) download(name string) string {
	for _, asset := range release.Assets {
		if asset.Name == name && asset.State == "uploaded" {
			return name
		}
	}
	return ""
}

type releaseCheck struct {
	mu      sync.Mutex
	client  *http.Client
	next    time.Time
	release stableRelease
	err     error
}

var publishedRelease = releaseCheck{client: &http.Client{
	Timeout: 6 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}}

func (check *releaseCheck) latest() (stableRelease, error) {
	check.mu.Lock()
	defer check.mu.Unlock()
	if time.Now().Before(check.next) {
		return check.release, check.err
	}
	check.release, check.err = fetchStableRelease(check.client)
	interval := 15 * time.Minute
	if check.err != nil {
		interval = time.Minute
	}
	check.next = time.Now().Add(interval)
	return check.release, check.err
}

func fetchStableRelease(client *http.Client) (stableRelease, error) {
	req, err := http.NewRequest(http.MethodGet, latestReleaseAPI, nil)
	if err != nil {
		return stableRelease{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "HarborMail")
	res, err := client.Do(req)
	if err != nil {
		return stableRelease{}, err
	}
	defer res.Body.Close()
	var release stableRelease
	if res.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&release) != nil {
		return stableRelease{}, errors.New("GitHub release unavailable")
	}
	if _, ok := stableVersion(release.Tag); !ok || release.Draft || release.Prerelease {
		return stableRelease{}, errors.New("invalid stable release")
	}
	return release, nil
}

func updateStatus(current, latest string) string {
	installed, ok := stableVersion(current)
	if !ok {
		return "development"
	}
	published, _ := stableVersion(latest)
	for i := range installed {
		switch cmp.Compare(installed[i], published[i]) {
		case -1:
			return "available"
		case 1:
			return "ahead"
		}
	}
	return "current"
}

func (a *app) adminCheckVersion(w http.ResponseWriter, _ *http.Request, _ string) {
	latest, err := publishedRelease.latest()
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "暂时无法访问 GitHub，请稍后重试。")
		return
	}
	status := updateStatus(currentBuildVersion(), latest.Tag)
	_, asset := deploymentDetails(runtime.GOOS, runtime.GOARCH, buildDeployment)
	reply(w, http.StatusOK, struct {
		Latest string `json:"latest"`
		Status string `json:"status"`
		Docker bool   `json:"docker"`
		Asset  string `json:"asset,omitempty"`
	}{latest.Tag, status, buildDeployment == "docker", latest.download(asset)})
}
