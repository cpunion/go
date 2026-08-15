// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package coroobj defines the private object metadata shared by the compiler
// and linker for the coroutine experiment.
package coroobj

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const (
	// Header identifies an object containing coroutine-owned native code.
	Header = "coro object v1"

	// Section identifies the JSON manifest in the textual Go object prefix.
	Section = "coro"

	// Version is the current manifest format version.
	Version = 1

	maxManifestSize = 1 << 20
	maxSymbols      = 4096
	maxNameLength   = 4096
)

// Manifest describes the native definitions contributed by one Go package.
type Manifest struct {
	Version int      `json:"version"`
	GOOS    string   `json:"goos"`
	GOARCH  string   `json:"goarch"`
	Symbols []Symbol `json:"symbols"`
}

// Symbol maps one Go ABI symbol to a definition in a native object member.
type Symbol struct {
	GoName   string `json:"goName"`
	GoABI    int    `json:"goABI"`
	HostName string `json:"hostName"`
}

// Encode validates m and writes its canonical JSON representation to w.
func Encode(w io.Writer, m *Manifest) error {
	if err := Validate(m); err != nil {
		return err
	}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if len(data) > maxManifestSize {
		return fmt.Errorf("manifest is %d bytes, maximum is %d", len(data), maxManifestSize)
	}
	n, err := w.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	return err
}

// Decode reads and validates one manifest from r.
func Decode(r io.Reader) (*Manifest, error) {
	limited := &io.LimitedReader{R: r, N: maxManifestSize + 1}
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	var m Manifest
	if err := decoder.Decode(&m); err != nil {
		if limited.N == 0 {
			return nil, fmt.Errorf("manifest is larger than %d bytes", maxManifestSize)
		}
		return nil, err
	}
	var extra any
	decodeErr := decoder.Decode(&extra)
	if limited.N == 0 {
		return nil, fmt.Errorf("manifest is larger than %d bytes", maxManifestSize)
	}
	if decodeErr != io.EOF {
		if decodeErr == nil {
			return nil, fmt.Errorf("multiple JSON values")
		}
		return nil, decodeErr
	}
	if err := Validate(&m); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate reports whether m is safe and internally consistent.
func Validate(m *Manifest) error {
	if m == nil {
		return fmt.Errorf("nil manifest")
	}
	if m.Version != Version {
		return fmt.Errorf("manifest version %d, want %d", m.Version, Version)
	}
	if err := validateName("goos", m.GOOS); err != nil {
		return err
	}
	if err := validateName("goarch", m.GOARCH); err != nil {
		return err
	}
	if len(m.Symbols) == 0 {
		return fmt.Errorf("manifest has no symbols")
	}
	if len(m.Symbols) > maxSymbols {
		return fmt.Errorf("manifest has %d symbols, maximum is %d", len(m.Symbols), maxSymbols)
	}

	goNames := make(map[string]struct{}, len(m.Symbols))
	hostNames := make(map[string]struct{}, len(m.Symbols))
	for i, symbol := range m.Symbols {
		if err := validateName(fmt.Sprintf("symbols[%d].goName", i), symbol.GoName); err != nil {
			return err
		}
		if symbol.GoABI != 0 && symbol.GoABI != 1 {
			return fmt.Errorf("symbols[%d].goABI is %d, want 0 or 1", i, symbol.GoABI)
		}
		if err := validateName(fmt.Sprintf("symbols[%d].hostName", i), symbol.HostName); err != nil {
			return err
		}
		key := fmt.Sprintf("%s<%d>", symbol.GoName, symbol.GoABI)
		if _, ok := goNames[key]; ok {
			return fmt.Errorf("duplicate Go symbol %s", key)
		}
		goNames[key] = struct{}{}
		if _, ok := hostNames[symbol.HostName]; ok {
			return fmt.Errorf("duplicate host symbol %q", symbol.HostName)
		}
		hostNames[symbol.HostName] = struct{}{}
	}
	return nil
}

func validateName(field, value string) error {
	if value == "" {
		return fmt.Errorf("%s is empty", field)
	}
	if len(value) > maxNameLength {
		return fmt.Errorf("%s is longer than %d bytes", field, maxNameLength)
	}
	if strings.ContainsAny(value, "\x00\r\n") {
		return fmt.Errorf("%s contains a control character", field)
	}
	return nil
}
