//go:build linux

package updater

import (
	"context"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLegacyCheckoutOwnerRequiresDirectoriesWithOneOwner(t *testing.T) {
	checkout := t.TempDir()
	if err := os.Mkdir(filepath.Join(checkout, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	owner, err := legacyCheckoutOwner(checkout)
	if err != nil || owner != uint32(os.Geteuid()) {
		t.Fatalf("legacyCheckoutOwner() = %d, %v", owner, err)
	}

	for name, prepare := range map[string]func(string) error{
		"missing": func(string) error { return nil },
		"file": func(gitPath string) error {
			return os.WriteFile(gitPath, []byte("gitdir: /elsewhere\n"), 0o644)
		},
		"symlink": func(gitPath string) error { return os.Symlink(t.TempDir(), gitPath) },
	} {
		t.Run(name, func(t *testing.T) {
			checkout := t.TempDir()
			if err := prepare(filepath.Join(checkout, ".git")); err != nil {
				t.Fatal(err)
			}
			if _, err := legacyCheckoutOwner(checkout); err == nil {
				t.Fatal("legacyCheckoutOwner() accepted a checkout without a .git directory")
			}
		})
	}
}

func TestLegacyGitRunsAsCheckoutOwnerWithoutRunningItsConfigAsRoot(t *testing.T) {
	nobody := requireRootWithGit(t)
	checkout, markers := makeHostileLegacyCheckout(t)
	chownTreeTo(t, checkout, nobody)

	ctx := context.Background()
	origin, err := runLegacyGit(ctx, checkout, "remote", "get-url", "origin")
	if err != nil || origin != "https://github.com/Yosodog/Nexus.git" {
		t.Fatalf("origin = %q, %v", origin, err)
	}
	status, err := runLegacyGit(ctx, checkout, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil || status != "" {
		t.Fatalf("status = %q, %v", status, err)
	}
	tag, err := runLegacyGit(ctx, checkout, "describe", "--tags", "--exact-match", "HEAD")
	if err != nil || tag != "v1.2.3" {
		t.Fatalf("tag = %q, %v", tag, err)
	}

	uid, err := os.ReadFile(filepath.Join(markers, "uid"))
	if err != nil {
		t.Fatalf("the checkout's clean filter did not run, so the test proves nothing: %v", err)
	}
	if strings.TrimSpace(string(uid)) != nobody.Uid {
		t.Fatalf("clean filter ran as uid %q, want %s", strings.TrimSpace(string(uid)), nobody.Uid)
	}
	groups, err := os.ReadFile(filepath.Join(markers, "groups"))
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range strings.Fields(string(groups)) {
		if group == "0" {
			t.Fatalf("clean filter kept root's group: %q", groups)
		}
	}
	if _, err := os.Stat(filepath.Join(markers, "fsmonitor")); err == nil {
		t.Fatal("the checkout's fsmonitor hook ran")
	}
}

func TestLegacyGitStillReadsRootOwnedCheckouts(t *testing.T) {
	requireRootWithGit(t)
	checkout, _ := makeHostileLegacyCheckout(t)
	tag, err := runLegacyGit(context.Background(), checkout, "describe", "--tags", "--exact-match", "HEAD")
	if err != nil || tag != "v1.2.3" {
		t.Fatalf("tag = %q, %v", tag, err)
	}
}

func TestLegacyGitRejectsCheckoutWithSplitOwnership(t *testing.T) {
	nobody := requireRootWithGit(t)
	checkout, _ := makeHostileLegacyCheckout(t)
	if err := os.Chown(checkout, mustAtoi(t, nobody.Uid), mustAtoi(t, nobody.Gid)); err != nil {
		t.Fatal(err)
	}
	_, err := runLegacyGit(context.Background(), checkout, "remote", "get-url", "origin")
	if err == nil || !strings.Contains(err.Error(), "different owners") {
		t.Fatalf("runLegacyGit() error = %v", err)
	}
}

func requireRootWithGit(t *testing.T) *user.User {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root to run Git as another user")
	}
	if _, err := os.Stat("/usr/bin/git"); err != nil {
		t.Skip("needs /usr/bin/git")
	}
	nobody, err := user.Lookup("nobody")
	if err != nil {
		t.Skip("needs a nobody account")
	}
	return nobody
}

// makeHostileLegacyCheckout creates a release checkout whose own config runs a
// clean filter and an fsmonitor hook that record who ran them.
func makeHostileLegacyCheckout(t *testing.T) (string, string) {
	t.Helper()
	base := t.TempDir()
	for _, directory := range []string{filepath.Dir(base), base} {
		if err := os.Chmod(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	checkout := filepath.Join(base, "checkout")
	markers := filepath.Join(base, "markers")
	if err := os.Mkdir(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(markers, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(markers, 0o777); err != nil {
		t.Fatal(err)
	}
	git := func(arguments ...string) {
		t.Helper()
		command := exec.Command("/usr/bin/git", append([]string{"-C", checkout, "-c", "user.name=Test", "-c", "user.email=test@example.com"}, arguments...)...)
		command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", arguments, err, output)
		}
	}
	tracked := filepath.Join(checkout, "tracked")
	if err := os.WriteFile(tracked, []byte("release\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("init", "-q")
	git("remote", "add", "origin", "https://github.com/Yosodog/Nexus.git")
	git("add", "tracked")
	git("commit", "-q", "-m", "Release")
	git("tag", "v1.2.3")

	hook := filepath.Join(checkout, ".git", "probe-fsmonitor")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nid -u > "+filepath.Join(markers, "fsmonitor")+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	git("config", "core.fsmonitor", hook)
	git("config", "filter.probe.clean", "sh -c 'id -u > "+filepath.Join(markers, "uid")+"; id -G > "+filepath.Join(markers, "groups")+"; cat'")
	if err := os.WriteFile(filepath.Join(checkout, ".git", "info", "attributes"), []byte("tracked filter=probe\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A changed mtime makes git status hash the file again, through the filter.
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(tracked, later, later); err != nil {
		t.Fatal(err)
	}
	return checkout, markers
}

func chownTreeTo(t *testing.T, root string, account *user.User) {
	t.Helper()
	uid, gid := mustAtoi(t, account.Uid), mustAtoi(t, account.Gid)
	err := filepath.Walk(root, func(path string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(path, uid, gid)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func mustAtoi(t *testing.T, value string) int {
	t.Helper()
	parsed, err := strconv.Atoi(value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
