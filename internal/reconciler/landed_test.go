package reconciler

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseLandWatch(t *testing.T) {
	out := `chainguard-dev/mono#7 url=u state=MERGED review=APPROVED armed=true gate_pending=false reviews=1 comments=0 fail=none landed=yes
chainguard-dev/mono#7 stage=staging status=success run="Deploy":success:u
chainguard-dev/mono#70 url=u state=MERGED review=APPROVED armed=true gate_pending=false reviews=1 comments=0 fail=none landed=no
chainguard-dev/mono#70 stage=staging status=pending
chainguard-dev/mono#8 url=u state=OPEN review=NONE armed=false gate_pending=false reviews=0 comments=0 fail=none
summary open=1 merged_ok=1 merged_failed=0 pending_deploy=1 waiting_release=0 landed=1
`
	tests := []struct {
		pr       int
		verified bool
		found    bool
	}{
		{pr: 7, verified: true, found: true},
		{pr: 70, found: true},
		{pr: 8, found: true},
		{pr: 9},
	}
	for _, tt := range tests {
		verified, detail := parseLandWatch(out, "chainguard-dev/mono", tt.pr)
		if verified != tt.verified || (detail != "") != tt.found {
			t.Errorf("PR %d: verified=%t detail=%q, want verified=%t found=%t", tt.pr, verified, detail, tt.verified, tt.found)
		}
	}
}

func TestLandWatchVerifiedWithoutBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	verified, detail, err := landWatchVerified(t.Context(), "chainguard-dev/mono", 7)
	if verified || err != nil || detail != "land-watch is not installed" {
		t.Fatalf("landWatchVerified() = %t, %q, %v", verified, detail, err)
	}
}

func TestLandWatchVerifiedRunsBinary(t *testing.T) {
	tests := []struct {
		name     string
		script   string
		verified bool
		wantErr  bool
	}{
		{name: "landed", script: "echo 'o/r#7 url=u state=MERGED landed=yes'", verified: true},
		{name: "failed stage exits 1", script: "echo 'o/r#7 url=u state=MERGED landed=no'; echo 'o/r#7 stage=staging status=failure'; exit 1"},
		{name: "crash", script: "echo boom >&2; exit 2", wantErr: true},
		{name: "no line for the PR", script: "echo 'summary landed=0'", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			// land-watch is called as: land-watch --cache FILE PRS_FILE.
			script := "#!/bin/sh\n[ \"$1\" = --cache ] && [ -f \"$3\" ] || { echo bad args >&2; exit 3; }\n" + tt.script + "\n"
			if err := os.WriteFile(filepath.Join(dir, "land-watch"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
			verified, _, err := landWatchVerified(t.Context(), "o/r", 7)
			if verified != tt.verified || (err != nil) != tt.wantErr {
				t.Fatalf("landWatchVerified() = %t, %v; want %t, err=%t", verified, err, tt.verified, tt.wantErr)
			}
		})
	}
}
