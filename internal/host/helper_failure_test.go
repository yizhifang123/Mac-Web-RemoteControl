package host

import (
	"io"
	"os/exec"
	"strings"
	"testing"
)

// The capture helper signals "no capturable display" with exit code 3
// (capture/main.swift); helperFailure must turn that into the viewer-legible
// message and leave every other exit as the raw read error.
func TestHelperFailureMapsNoDisplayExit(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 3")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	err := helperFailure(cmd, io.EOF)
	if err == nil || !strings.Contains(err.Error(), "no capturable display") {
		t.Fatalf("exit 3 not mapped to the no-display message, got: %v", err)
	}
}

func TestHelperFailureLeavesOtherExitsAlone(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	err := helperFailure(cmd, io.EOF)
	if err == nil || !strings.Contains(err.Error(), "read frame header") {
		t.Fatalf("exit 1 should surface the read error, got: %v", err)
	}
}
