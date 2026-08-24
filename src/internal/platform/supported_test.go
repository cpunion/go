// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package platform_test

import (
	"internal/platform"
	"testing"
)

func TestCoroDirectSupported(t *testing.T) {
	tests := []struct {
		goos, goarch string
		want         bool
	}{
		{"darwin", "arm64", true},
		{"linux", "amd64", true},
		{"darwin", "amd64", false},
		{"linux", "arm64", false},
		{"windows", "amd64", false},
	}
	for _, test := range tests {
		if got := platform.CoroDirectSupported(test.goos, test.goarch); got != test.want {
			t.Errorf("CoroDirectSupported(%q, %q) = %t, want %t",
				test.goos, test.goarch, got, test.want)
		}
	}
}
