package main

import (
	"strings"
	"testing"
)

func TestDoctorIOSBestEffortNote(t *testing.T) {
	cases := []struct {
		name string
		rep  doctorReport
		want bool
	}{
		{"forced ios range check healthy warns", doctorReport{Healthy: true, ForcedIOS: true}, true},
		{"forced ios full run already proved delivery", doctorReport{Healthy: true, Full: true, ForcedIOS: true}, false},
		{"forced ios unhealthy has its own error", doctorReport{Healthy: false, ForcedIOS: true}, false},
		{"not forced ios no note", doctorReport{Healthy: true, ForcedIOS: false}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			note := doctorIOSBestEffortNote(&tc.rep)
			if (note != "") != tc.want {
				t.Fatalf("note = %q, want non-empty=%v", note, tc.want)
			}
			if tc.want && !strings.Contains(note, "media delivery is unreliable") {
				t.Errorf("note = %q, want it to warn that iOS media delivery is unreliable", note)
			}
		})
	}
}
