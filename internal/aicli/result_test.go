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
	if !utf8.ValidString(message) || len(message) > bounded.DiagnosticBytes || !strings.HasSuffix(message, "...") {
		t.Fatalf("unsafe diagnostic boundary: valid=%t bytes=%d tail=%q", utf8.ValidString(message), len(message), message[len(message)-min(len(message), 12):])
	}
}
