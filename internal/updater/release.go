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

// Version is a Nexus release tag: vMAJOR.MINOR.PATCH with an optional
// semantic-version prerelease suffix such as -beta.1 or -rc.2.
type Version struct {
	Major      int
	Minor      int
	Patch      int
	Prerelease string
}

func ParseVersion(tag string) (Version, error) {
	if len(tag) < 2 || tag[0] != 'v' || strings.Contains(tag, "+") {
		return Version{}, fmt.Errorf("release tag %q is not a Nexus version", tag)
	}
	core, prerelease, hasPrerelease := strings.Cut(tag[1:], "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("release tag %q must use vMAJOR.MINOR.PATCH", tag)
	}
	values := make([]int, 3)
	for index, part := range parts {
		value, err := parseNumericIdentifier(part)
		if err != nil {
			return Version{}, fmt.Errorf("release tag %q is invalid", tag)
		}
		values[index] = value
	}
	if hasPrerelease {
		if prerelease == "" || len(prerelease) > 64 {
			return Version{}, fmt.Errorf("release tag %q has an invalid prerelease", tag)
		}
		for _, identifier := range strings.Split(prerelease, ".") {
			if !validPrereleaseIdentifier(identifier) {
				return Version{}, fmt.Errorf("release tag %q has an invalid prerelease", tag)
			}
		}
	}
	return Version{Major: values[0], Minor: values[1], Patch: values[2], Prerelease: prerelease}, nil
}

func parseNumericIdentifier(value string) (int, error) {
	if value == "" || (len(value) > 1 && value[0] == '0') || len(value) > 9 {
		return 0, errors.New("invalid numeric identifier")
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return 0, errors.New("invalid numeric identifier")
		}
	}
	return strconv.Atoi(value)
}

func validPrereleaseIdentifier(identifier string) bool {
	if identifier == "" {
		return false
	}
	numeric := true
	for _, character := range identifier {
		switch {
		case character >= '0' && character <= '9':
		case (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z'):
			numeric = false
		default:
			return false
		}
	}
	return !numeric || identifier == "0" || identifier[0] != '0'
}

// Stable reports whether the version has no prerelease suffix.
func (version Version) Stable() bool {
	return version.Prerelease == ""
}

// Compare orders versions by semantic-version precedence: a prerelease sorts
// before the stable release with the same MAJOR.MINOR.PATCH.
func (version Version) Compare(other Version) int {
	if version.Major != other.Major {
		return compareInt(version.Major, other.Major)
	}
	if version.Minor != other.Minor {
		return compareInt(version.Minor, other.Minor)
	}
	if version.Patch != other.Patch {
		return compareInt(version.Patch, other.Patch)
	}
	return comparePrerelease(version.Prerelease, other.Prerelease)
}

func comparePrerelease(left, right string) int {
	if left == right {
		return 0
	}
	if left == "" {
		return 1
	}
	if right == "" {
		return -1
	}
	leftParts := strings.Split(left, ".")
	rightParts := strings.Split(right, ".")
	for index := 0; index < len(leftParts) && index < len(rightParts); index++ {
		leftNumber, leftErr := strconv.Atoi(leftParts[index])
		rightNumber, rightErr := strconv.Atoi(rightParts[index])
		switch {
		case leftErr == nil && rightErr == nil:
			if leftNumber != rightNumber {
				return compareInt(leftNumber, rightNumber)
			}
		case leftErr == nil:
			return -1
		case rightErr == nil:
			return 1
		default:
			if comparison := strings.Compare(leftParts[index], rightParts[index]); comparison != 0 {
				return comparison
			}
		}
	}
	return compareInt(len(leftParts), len(rightParts))
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

// CompareTags compares two release tags that have already been validated.
func CompareTags(left, right string) int {
	leftVersion, _ := ParseVersion(left)
	rightVersion, _ := ParseVersion(right)
	return leftVersion.Compare(rightVersion)
}

// Channel selects which published releases a host follows.
type Channel string

const (
	ChannelStable Channel = "stable"
	ChannelBeta   Channel = "beta"
)

func validChannel(channel Channel) bool {
	return channel == ChannelStable || channel == ChannelBeta
}

// releaseOnChannel reports whether a release is visible to a channel. Stable
// hosts see only stable tags that GitHub does not mark as prereleases; beta
// hosts see stable releases and every prerelease.
func releaseOnChannel(tag string, githubPrerelease bool, channel Channel) bool {
	version, err := ParseVersion(tag)
	if err != nil {
		return false
	}
	if channel == ChannelBeta {
		return true
	}
	return version.Stable() && !githubPrerelease
}

type ReleaseInfo struct {
	Tag         string    `json:"release_id"`
	Name        string    `json:"name,omitempty"`
	Notes       string    `json:"release_notes,omitempty"`
	PublishedAt time.Time `json:"published_at"`
}

type ReleaseProvider interface {
	Releases(context.Context, string, Channel) ([]ReleaseInfo, error)
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

// Releases lists the published releases of one official repository that are
// visible on the channel, oldest first.
func (client GitHubReleaseClient) Releases(ctx context.Context, repository string, channel Channel) ([]ReleaseInfo, error) {
	if !validRepository(repository) {
		return nil, errors.New("unsupported release repository")
	}
	if !validChannel(channel) {
		return nil, errors.New("unsupported release channel")
	}
	requestContext, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	httpClient := client.Client
	if httpClient == nil {
		httpClient = NewGitHubReleaseClient().Client
	}
	var releases []ReleaseInfo
	for page := 1; page <= maxReleasePages; page++ {
		endpoint := fmt.Sprintf("https://api.github.com/repos/Yosodog/%s/releases?per_page=100&page=%d", repository, page)
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
			if release.Draft || !releaseOnChannel(release.TagName, release.Prerelease, channel) {
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
	sortReleases(releases)
	return releases, nil
}

func sortReleases(releases []ReleaseInfo) {
	sort.Slice(releases, func(left, right int) bool {
		return CompareTags(releases[left].Tag, releases[right].Tag) < 0
	})
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

func validRepository(repository string) bool {
	switch repository {
	case setupRepository, coreRepository, subsRepository, discordRepository:
		return true
	default:
		return false
	}
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
