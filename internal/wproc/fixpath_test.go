package wproc

import (
	"strings"
	"testing"
)

func TestFixPathWindowsForms(t *testing.T) {
	longTail := strings.Repeat(`nested\`, 35) + "image.jpg"
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "short drive", in: `C:\media\image.jpg`, want: `C:\media\image.jpg`},
		{name: "long drive", in: `C:\` + longTail, want: `\\?\C:\` + longTail},
		{name: "long UNC", in: `\\server\share\` + longTail, want: `\\?\UNC\server\share\` + longTail},
		{name: "already prefixed drive", in: `\\?\C:\` + longTail, want: `\\?\C:\` + longTail},
		{name: "already prefixed UNC", in: `\\?\UNC\server\share\` + longTail, want: `\\?\UNC\server\share\` + longTail},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := fixPath(tc.in); got != tc.want {
				t.Fatalf("fixPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
