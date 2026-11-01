package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		wantCode int
		wantOut  string
		wantErr  string
	}{
		{name: "version", args: []string{"version"}, wantCode: 0, wantOut: "hvmc dev\n"},
		{name: "flag", args: []string{"--version"}, wantCode: 0, wantOut: "hvmc dev\n"},
		{name: "no args", args: nil, wantCode: 2, wantErr: "usage"},
		{name: "unknown", args: []string{"serve"}, wantCode: 2, wantErr: "usage"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := run(tc.args, &out, &errOut); code != tc.wantCode {
				t.Fatalf("exit code = %d, want %d", code, tc.wantCode)
			}
			if out.String() != tc.wantOut {
				t.Errorf("stdout = %q, want %q", out.String(), tc.wantOut)
			}
			if !strings.Contains(errOut.String(), tc.wantErr) {
				t.Errorf("stderr = %q, want it to contain %q", errOut.String(), tc.wantErr)
			}
		})
	}
}
