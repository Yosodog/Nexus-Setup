package main

import (
	"bufio"
	"strings"
	"testing"
)

func TestCommandHelpListsPublicCommands(t *testing.T) {
	help, err := commandHelp("")
	if err != nil {
		t.Fatal(err)
	}

	for _, command := range []string{"install", "update", "status", "doctor", "rollback", "cleanup", "component", "operation", "version"} {
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
		"1", "nexus.example.com", "admin@example.com", "long-password", "123", "456", "api-key", "mutation-key",
	}, "\n") + "\n"))

	options, err := promptInstallOptions()
	if err != nil {
		t.Fatal(err)
	}
	if options.PWAPIKey != "api-key" || options.PWMutationKey != "mutation-key" {
		t.Fatal("Politics & War keys were not collected separately")
	}
}
