package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/coltconsulting/click-dog/internal/updater"
)

func TestWriteSelfUpdateVerificationMode(t *testing.T) {
	tests := []struct {
		name       string
		mode       updater.ReleaseVerificationMode
		wantOut    string
		wantErrOut string
	}{
		{
			name:    "signed",
			mode:    updater.ReleaseVerificationSigned,
			wantOut: "Publisher signature: verified with cosign. Archive SHA-256 matched.",
		},
		{
			name:       "cosign missing",
			mode:       updater.ReleaseVerificationNoCosign,
			wantErrOut: "WARNING: cosign is not installed; publisher signature was not verified. Archive SHA-256 matched.",
		},
		{
			name:       "dangerous override",
			mode:       updater.ReleaseVerificationCosignIgnored,
			wantErrOut: "DANGER: publisher signature deliberately skipped by --dangerously-ignore-cosign. Archive SHA-256 matched.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			writeSelfUpdateVerificationMode(&out, &errOut, tt.mode, true)
			if tt.wantOut == "" && out.Len() != 0 {
				t.Errorf("stdout = %q, want it to be empty", out.String())
			} else if tt.wantOut != "" && !strings.Contains(out.String(), tt.wantOut) {
				t.Errorf("stdout = %q, want it to contain %q", out.String(), tt.wantOut)
			}
			if tt.wantErrOut == "" && errOut.Len() != 0 {
				t.Errorf("stderr = %q, want it to be empty", errOut.String())
			} else if tt.wantErrOut != "" && !strings.Contains(errOut.String(), tt.wantErrOut) {
				t.Errorf("stderr = %q, want it to contain %q", errOut.String(), tt.wantErrOut)
			}
		})
	}
}

func TestWriteSelfUpdateVerificationModePrintsRemediationOnlyBeforeDownload(t *testing.T) {
	const docsURL = "https://docs.sigstore.dev/cosign/system_config/installation/"

	var out, errOut bytes.Buffer
	writeSelfUpdateVerificationMode(&out, &errOut, updater.ReleaseVerificationNoCosign, false)
	if !strings.Contains(errOut.String(), docsURL) {
		t.Errorf("initial warning = %q, want it to contain %q", errOut.String(), docsURL)
	}

	out.Reset()
	errOut.Reset()
	writeSelfUpdateVerificationMode(&out, &errOut, updater.ReleaseVerificationNoCosign, true)
	if strings.Contains(errOut.String(), docsURL) {
		t.Errorf("closing summary = %q, want remediation URL omitted", errOut.String())
	}
}

func TestSelfUpdateHelpDocumentsDangerousCosignOverride(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := runSelfUpdate([]string{"--help"}, &out, &errOut); code != 0 {
		t.Fatalf("runSelfUpdate(--help) = %d, want 0", code)
	}
	if !strings.Contains(errOut.String(), "dangerously-ignore-cosign") {
		t.Errorf("help = %q, want dangerous cosign override documented", errOut.String())
	}
}
