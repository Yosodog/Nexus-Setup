package main

import (
	"bufio"
	"strings"
	"testing"

	"github.com/Yosodog/Nexus-Setup/internal/updater"
)

func TestCommandHelpListsPublicCommands(t *testing.T) {
	help, err := commandHelp("")
	if err != nil {
		t.Fatal(err)
	}

	for _, command := range []string{"install", "update", "status", "doctor", "rollback", "cleanup", "component", "channel", "operation", "version"} {
		if !strings.Contains(help, command) {
			t.Fatalf("expected help to list %q", command)
		}
	}
}

func TestCommandHelpExplainsComponentRestrictions(t *testing.T) {
	help, err := commandHelp("component")
	if err != nil {
		t.Fatal(err)
	}

	for _, expected := range []string{"install <subs|discord>", "restart <core|subs|discord>"} {
		if !strings.Contains(help, expected) {
			t.Fatalf("expected component help to contain %q", expected)
		}
	}
}

func TestCommandHelpRejectsUnknownTopics(t *testing.T) {
	if _, err := commandHelp("anything"); err == nil {
		t.Fatal("expected an unknown help topic to fail")
	}
}

func TestInstallPromptReadsPWMutationKey(t *testing.T) {
	previous := stdinReader
	defer func() { stdinReader = previous }()
	stdinReader = bufio.NewReader(strings.NewReader(strings.Join([]string{
		"1", "nexus.example.com", "admin@example.com", "long-password", "long-password", "123", "456", "api-key", "mutation-key",
	}, "\n") + "\n"))

	options, err := promptInstallOptions()
	if err != nil {
		t.Fatal(err)
	}
	if options.PWAPIKey != "api-key" || options.PWMutationKey != "mutation-key" {
		t.Fatal("Politics & War keys were not collected separately")
	}
}

func TestChannelCommandBuildsSetChannelRequest(t *testing.T) {
	request, err := commandRequest([]string{"channel", "beta"})
	if err != nil {
		t.Fatal(err)
	}
	if request.Operation != updater.OperationSetChannel || request.Channel != updater.ChannelBeta || request.OperationID == "" {
		t.Fatalf("unexpected channel request: %+v", request)
	}
	if _, err := commandRequest([]string{"channel", "nightly"}); err == nil {
		t.Fatal("unknown channel was accepted")
	}
}

func TestRollbackCommandAcceptsComponent(t *testing.T) {
	request, err := commandRequest([]string{"rollback", "subs"})
	if err != nil {
		t.Fatal(err)
	}
	if request.Operation != updater.OperationRollback || request.Component != updater.ComponentSubs {
		t.Fatalf("unexpected rollback request: %+v", request)
	}
	if _, err := commandRequest([]string{"rollback", "web"}); err == nil {
		t.Fatal("unknown component rollback was accepted")
	}
}

func TestInstallPromptAsksAgainAfterInvalidAnswers(t *testing.T) {
	previous := stdinReader
	defer func() { stdinReader = previous }()
	stdinReader = bufio.NewReader(strings.NewReader(strings.Join([]string{
		"9", "1",
		"https://nexus.example.com", "nexus.example.com",
		"admin", "admin@example.com",
		"short", "long-password", "different-password", "long-password", "long-password",
		"Yoso", "123",
		"456",
		"", "api-key",
		"mutation-key",
	}, "\n") + "\n"))

	options, err := promptInstallOptions()
	if err != nil {
		t.Fatal(err)
	}
	if options.Profile != updater.ProfileFull || options.Domain != "nexus.example.com" || options.AdminEmail != "admin@example.com" ||
		options.AdminPassword != "long-password" || options.AdminNationID != "123" || options.PWAPIKey != "api-key" {
		t.Fatalf("unexpected options after retries: %+v", options)
	}
}

func TestInstallPromptStopsAfterRepeatedInvalidAnswers(t *testing.T) {
	previous := stdinReader
	defer func() { stdinReader = previous }()
	stdinReader = bufio.NewReader(strings.NewReader(""))
	if _, err := promptInstallOptions(); err == nil || !strings.Contains(err.Error(), "Domain") {
		t.Fatalf("expected the domain prompt to give up, got %v", err)
	}
}
