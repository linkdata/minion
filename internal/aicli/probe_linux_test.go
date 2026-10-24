//go:build linux

package aicli

import (
	"bytes"
	"os/exec"
	"testing"
)

func TestProbePreservesLogicalArgv0(t *testing.T) {
	requireProbeProcessGroup(t)
	cat, err := exec.LookPath("cat")
	if err != nil {
		t.Skip("cat is unavailable")
	}
	output, err := Probe(t.Context(), cat, "logical-name", []string{"/proc/self/cmdline"}, t.TempDir(), nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(output, []byte("logical-name\x00")) {
		t.Fatalf("probe argv0 = %q", output)
	}
}
