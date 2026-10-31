package aicli

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/linkdata/minion/internal/bounded"
)

func TestProviderDiagnosticsKeepSafeBoundAndOverflowEvidence(t *testing.T) {
	var diagnostics []ProviderDiagnostic
	for i := range 100 {
		diagnostics = AppendNativeDiagnostic(diagnostics, "native_error", string(rune('a'+i%26)))
	}
	if len(diagnostics) != 62 || diagnostics[0].Code != "diagnostics_truncated" || diagnostics[61].Message != string(rune('a'+99%26)) {
		t.Fatalf("native observations lost overflow marker: %+v", diagnostics)
	}
	message := DiagnosticText(strings.Repeat("x", bounded.DiagnosticBytes-2) + "€" + "\xff")
	if !utf8.ValidString(message) || len(message) > bounded.DiagnosticBytes || !strings.Contains(message, "...") || !strings.HasSuffix(message, "€�") {
		t.Fatalf("unsafe diagnostic boundary: valid=%t bytes=%d tail=%q", utf8.ValidString(message), len(message), message[len(message)-min(len(message), 12):])
	}
}

func TestDiagnosticTextKeepsTrailingQuotaReset(t *testing.T) {
	reset := "Session quota resets at 2026-11-01T01:20:00Z"
	message := DiagnosticText(strings.Repeat("x", bounded.DiagnosticBytes) + reset)
	if !utf8.ValidString(message) || len(message) > bounded.DiagnosticBytes || !strings.HasSuffix(message, reset) {
		t.Fatalf("trailing quota reset lost: length=%d tail=%q", len(message), message[len(message)-min(len(message), 80):])
	}
}

func TestDiagnosticTextKeepsBothCutsOnRuneBoundaries(t *testing.T) {
	const marker = "..."
	head := (bounded.DiagnosticBytes - len(marker)) / 2
	tail := bounded.DiagnosticBytes - len(marker) - head
	message := strings.Repeat("x", head-2) + "€" + strings.Repeat("m", 100) + "€" + strings.Repeat("y", tail-2)
	got := DiagnosticText(message)
	if !utf8.ValidString(got) || len(got) > bounded.DiagnosticBytes || !strings.Contains(got, marker) || !strings.HasSuffix(got, strings.Repeat("y", tail-2)) {
		t.Fatalf("diagnostic cut split a rune: length=%d tail=%q", len(got), got[len(got)-min(len(got), 12):])
	}
}
