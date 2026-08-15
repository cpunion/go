// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coroobj

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestManifestRoundTrip(t *testing.T) {
	want := &Manifest{
		Version: Version,
		GOOS:    "linux",
		GOARCH:  "amd64",
		Symbols: []Symbol{{
			GoName:   "example.com/p.leaf",
			GoABI:    1,
			HostName: "go_coro_0123456789abcdef",
		}},
	}
	var data bytes.Buffer
	if err := Encode(&data, want); err != nil {
		t.Fatal(err)
	}
	got, err := Decode(&data)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != want.Version || got.GOOS != want.GOOS || got.GOARCH != want.GOARCH || len(got.Symbols) != 1 || got.Symbols[0] != want.Symbols[0] {
		t.Fatalf("Decode(Encode(m)) = %#v, want %#v", got, want)
	}
}

func TestManifestRejectsInvalidInput(t *testing.T) {
	valid := `{"version":1,"goos":"linux","goarch":"amd64","symbols":[{"goName":"p.f","goABI":1,"hostName":"go_coro_f"}]}`
	tests := []struct {
		name string
		data string
		want string
	}{
		{"unknown field", strings.Replace(valid, `"goos"`, `"extra":true,"goos"`, 1), "unknown field"},
		{"wrong version", strings.Replace(valid, `"version":1`, `"version":2`, 1), "version 2"},
		{"missing target", strings.Replace(valid, `"goos":"linux"`, `"goos":""`, 1), "goos is empty"},
		{"bad ABI", strings.Replace(valid, `"goABI":1`, `"goABI":2`, 1), "want 0 or 1"},
		{"control character", strings.Replace(valid, `p.f`, `p.\nf`, 1), "control character"},
		{"duplicate JSON", valid + valid, "multiple JSON values"},
		{"truncated JSON", valid[:len(valid)-1], "unexpected EOF"},
		{"too large", valid + strings.Repeat(" ", maxManifestSize), "larger than"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Decode(strings.NewReader(test.data))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Decode error = %v, want error containing %q", err, test.want)
			}
		})
	}
}

func TestManifestRejectsInvalidValue(t *testing.T) {
	longName := strings.Repeat("x", maxNameLength+1)
	tests := []struct {
		name string
		m    *Manifest
		want string
	}{
		{"nil", nil, "nil manifest"},
		{"no symbols", &Manifest{Version: Version, GOOS: "linux", GOARCH: "amd64"}, "no symbols"},
		{"too many symbols", &Manifest{Version: Version, GOOS: "linux", GOARCH: "amd64", Symbols: make([]Symbol, maxSymbols+1)}, "maximum"},
		{"long target", &Manifest{Version: Version, GOOS: longName, GOARCH: "amd64", Symbols: []Symbol{{GoName: "p.f", HostName: "f"}}}, "longer than"},
		{"empty Go name", &Manifest{Version: Version, GOOS: "linux", GOARCH: "amd64", Symbols: []Symbol{{HostName: "f"}}}, "goName is empty"},
		{"empty host name", &Manifest{Version: Version, GOOS: "linux", GOARCH: "amd64", Symbols: []Symbol{{GoName: "p.f"}}}, "hostName is empty"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := Validate(test.m); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate error = %v, want error containing %q", err, test.want)
			}
		})
	}
}

func TestManifestEncodeErrors(t *testing.T) {
	if err := Encode(io.Discard, nil); err == nil || !strings.Contains(err.Error(), "nil manifest") {
		t.Fatalf("Encode error = %v, want nil manifest", err)
	}
	m := &Manifest{
		Version: Version,
		GOOS:    "linux",
		GOARCH:  "amd64",
		Symbols: []Symbol{{GoName: "p.f", GoABI: 1, HostName: "go_coro_f"}},
	}
	w := &failingWriter{err: errors.New("write failed")}
	if err := Encode(w, m); !errors.Is(err, w.err) {
		t.Fatalf("Encode error = %v, want %v", err, w.err)
	}
	w.err = nil
	if err := Encode(w, m); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("Encode error = %v, want %v", err, io.ErrShortWrite)
	}
	long := strings.Repeat("x", maxNameLength-8)
	m.Symbols = make([]Symbol, 130)
	for i := range m.Symbols {
		suffix := string(rune(0x1000 + i))
		m.Symbols[i] = Symbol{GoName: long + "g" + suffix, GoABI: 1, HostName: long + "h" + suffix}
	}
	if err := Encode(io.Discard, m); err == nil || !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("oversized Encode error = %v, want maximum size", err)
	}
}

type failingWriter struct {
	err error
}

func (w *failingWriter) Write([]byte) (int, error) {
	return 0, w.err
}

func TestManifestRejectsDuplicateSymbols(t *testing.T) {
	m := &Manifest{
		Version: Version,
		GOOS:    "darwin",
		GOARCH:  "arm64",
		Symbols: []Symbol{
			{GoName: "p.f", GoABI: 1, HostName: "go_coro_f"},
			{GoName: "p.f", GoABI: 1, HostName: "go_coro_g"},
		},
	}
	if err := Validate(m); err == nil || !strings.Contains(err.Error(), "duplicate Go symbol") {
		t.Fatalf("Validate error = %v, want duplicate Go symbol", err)
	}
	m.Symbols[1] = Symbol{GoName: "p.g", GoABI: 1, HostName: "go_coro_f"}
	if err := Validate(m); err == nil || !strings.Contains(err.Error(), "duplicate host symbol") {
		t.Fatalf("Validate error = %v, want duplicate host symbol", err)
	}
}
