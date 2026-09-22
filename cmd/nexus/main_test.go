package main

import (
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
