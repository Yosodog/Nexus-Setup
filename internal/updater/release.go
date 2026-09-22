package updater

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	setupRepository   = "Nexus-Setup"
	coreRepository    = "Nexus-AMS"
	subsRepository    = "Nexus-AMS-Subs"
	discordRepository = "Nexus-AMS-Discord"
	setupAsset        = "nexus-linux-amd64"
	coreAsset         = "nexus-core.tar.gz"
	subsAsset         = "nexus-subs.tar.gz"
	discordAsset      = "nexus-discord.tar.gz"
	maxReleasePages   = 5
	maxReleaseBody    = 2 * 1024 * 1024
)

type Version struct {
	Major int
	Minor int
	Patch int
}

func ParseVersion(tag string) (Version, error) {
	if len(tag) < 2 || tag[0] != 'v' || strings.ContainsAny(tag, "+-") {
		return Version{}, fmt.Errorf("release tag %q is not a stable Nexus version", tag)
	}
	parts := strings.Split(tag[1:], ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("release tag %q must use vMAJOR.MINOR.PATCH", tag)
	}
	values := make([]int, 3)
	for index, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return Version{}, fmt.Errorf("release tag %q is invalid", tag)
		}
		value, err := strconv.Atoi(part)
		if err != nil || value < 0 {
			return Version{}, fmt.Errorf("release tag %q is invalid", tag)
		}
		values[index] = value
	}
	return Version{Major: values[0], Minor: values[1], Patch: values[2]}, nil
}

func (version Version) Compare(other Version) int {
	if version.Major != other.Major {
		return compareInt(version.Major, other.Major)
	}
	if version.Minor != other.Minor {
		return compareInt(version.Minor, other.Minor)
	}
	return compareInt(version.Patch, other.Patch)
}

func compareInt(left, right int) int {
	if left < right {
		return -1
	}
	if left > right {
		return 1
	}
	return 0
}

type ReleaseInfo struct {
	Tag         string    `json:"release_id"`
	Name        string    `json:"name,omitempty"`
	Notes       string    `json:"release_notes,omitempty"`
	PublishedAt time.Time `json:"published_at"`
}

type ReleaseProvider interface {
	StableReleases(context.Context) ([]ReleaseInfo, error)
	Download(context.Context, string, string, string, string, int64, func(int64, int64)) error
}

type GitHubReleaseClient struct {
	Client *http.Client
}

type githubRelease struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Body        string    `json:"body"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
}

func NewGitHubReleaseClient() GitHubReleaseClient {
	return GitHubReleaseClient{Client: &http.Client{
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) > 5 {
				return errors.New("too many GitHub redirects")
			}
			host := strings.ToLower(request.URL.Hostname())
			if host != "github.com" && host != "api.github.com" && host != "objects.githubusercontent.com" && host != "release-assets.githubusercontent.com" {
				return errors.New("GitHub download redirected to an unexpected host")
			}
			return nil
		},
	}}
}

func (client GitHubReleaseClient) StableReleases(ctx context.Context) ([]ReleaseInfo, error) {
	requestContext, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	httpClient := client.Client
	if httpClient == nil {
		httpClient = NewGitHubReleaseClient().Client
	}
	var releases []ReleaseInfo
	for page := 1; page <= maxReleasePages; page++ {
		endpoint := fmt.Sprintf("https://api.github.com/repos/Yosodog/%s/releases?per_page=100&page=%d", setupRepository, page)
		request, err := http.NewRequestWithContext(requestContext, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		request.Header.Set("Accept", "application/vnd.github+json")
		request.Header.Set("User-Agent", "nexus-updater")
		response, err := httpClient.Do(request)
		if err != nil {
			return nil, err
		}
		pageReleases, err := decodeGitHubReleases(response)
		if err != nil {
			return nil, err
		}
		for _, release := range pageReleases {
			if release.Draft || release.Prerelease {
				continue
			}
			if _, err := ParseVersion(release.TagName); err != nil {
				continue
			}
			releases = append(releases, ReleaseInfo{
				Tag: release.TagName, Name: release.Name,
				Notes: normalizeReleaseNotes(release.Body), PublishedAt: release.PublishedAt,
			})
		}
		if len(pageReleases) < 100 {
			break
		}
	}
	sort.Slice(releases, func(left, right int) bool {
		leftVersion, _ := ParseVersion(releases[left].Tag)
		rightVersion, _ := ParseVersion(releases[right].Tag)
		return leftVersion.Compare(rightVersion) < 0
	})
	return releases, nil
}

func decodeGitHubReleases(response *http.Response) ([]githubRelease, error) {
	if response == nil {
		return nil, errors.New("GitHub returned no response")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return nil, fmt.Errorf("GitHub release request failed with status %d", response.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxReleaseBody+1))
	var releases []githubRelease
	if err := decoder.Decode(&releases); err != nil {
		return nil, fmt.Errorf("decode GitHub releases: %w", err)
	}
	if len(releases) > 100 {
		return nil, errors.New("GitHub returned too many releases")
	}
	return releases, nil
}

func normalizeReleaseNotes(notes string) string {
	notes = strings.ReplaceAll(notes, "\x00", "")
	notes = strings.TrimSpace(notes)
	if len(notes) > 64*1024 {
		return notes[:64*1024]
	}
	return notes
}

func UpdatesAfter(releases []ReleaseInfo, installed string) ([]ReleaseInfo, error) {
	installedVersion, err := ParseVersion(installed)
	if err != nil {
		return nil, err
	}
	updates := make([]ReleaseInfo, 0)
	for _, release := range releases {
		version, parseErr := ParseVersion(release.Tag)
		if parseErr != nil {
			continue
		}
		if version.Compare(installedVersion) > 0 {
			updates = append(updates, release)
		}
	}
	sort.Slice(updates, func(left, right int) bool {
		leftVersion, _ := ParseVersion(updates[left].Tag)
		rightVersion, _ := ParseVersion(updates[right].Tag)
		return leftVersion.Compare(rightVersion) < 0
	})
	previous := installedVersion
	for _, release := range updates {
		version, _ := ParseVersion(release.Tag)
		if !isImmediateSuccessor(previous, version) {
			return nil, fmt.Errorf("required intermediate release before %s is unavailable", release.Tag)
		}
		previous = version
	}
	return updates, nil
}

func isImmediateSuccessor(previous, next Version) bool {
	if next.Major == previous.Major && next.Minor == previous.Minor {
		return next.Patch == previous.Patch+1
	}
	if next.Major == previous.Major && next.Minor == previous.Minor+1 {
		return next.Patch == 0
	}
	return next.Major == previous.Major+1 && next.Minor == 0 && next.Patch == 0
}

func (client GitHubReleaseClient) Download(ctx context.Context, repository, tag, asset, destination string, maxBytes int64, progress func(int64, int64)) error {
	if !validRepositoryAsset(repository, asset) {
		return errors.New("unsupported release asset")
	}
	if _, err := ParseVersion(tag); err != nil {
		return err
	}
	if !filepath.IsAbs(destination) || maxBytes <= 0 {
		return errors.New("download destination or size limit is invalid")
	}
	endpoint := "https://github.com/Yosodog/" + repository + "/releases/download/" + url.PathEscape(tag) + "/" + asset
	downloadContext, cancel := context.WithTimeout(ctx, 90*time.Minute)
	defer cancel()
	request, err := http.NewRequestWithContext(downloadContext, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	request.Header.Set("User-Agent", "nexus-updater")
	httpClient := client.Client
	if httpClient == nil {
		httpClient = NewGitHubReleaseClient().Client
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub asset request failed with status %d", response.StatusCode)
	}
	if response.ContentLength > maxBytes {
		return errors.New("release asset exceeds the download limit")
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	remove := true
	defer func() {
		_ = file.Close()
		if remove {
			_ = os.Remove(destination)
		}
	}()
	reader := bufio.NewReader(io.LimitReader(response.Body, maxBytes+1))
	buffer := make([]byte, 128*1024)
	var written int64
	for {
		count, readErr := reader.Read(buffer)
		if count > 0 {
			written += int64(count)
			if written > maxBytes {
				return errors.New("release asset exceeds the download limit")
			}
			if _, err := file.Write(buffer[:count]); err != nil {
				return err
			}
			if progress != nil {
				progress(written, response.ContentLength)
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	remove = false
	return nil
}

func validRepositoryAsset(repository, asset string) bool {
	pairs := map[string]string{
		setupRepository:   setupAsset,
		coreRepository:    coreAsset,
		subsRepository:    subsAsset,
		discordRepository: discordAsset,
	}
	return pairs[repository] == asset
}

type ArtifactIdentity struct {
	SchemaVersion int       `json:"schema_version"`
	Component     Component `json:"component"`
	Release       string    `json:"release"`
}

func ValidateArtifactIdentity(directory string, component Component, release string) error {
	path := filepath.Join(directory, "nexus-release.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return errors.New("artifact identity file is missing")
	}
	if len(data) > 4096 {
		return errors.New("artifact identity file is too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var identity ArtifactIdentity
	if err := decoder.Decode(&identity); err != nil {
		return errors.New("artifact identity file is invalid")
	}
	if identity.SchemaVersion != 1 || identity.Component != component || identity.Release != release {
		return errors.New("artifact identity does not match the requested release")
	}
	return nil
}
