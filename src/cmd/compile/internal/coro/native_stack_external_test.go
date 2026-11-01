// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"debug/elf"
	"debug/macho"
	"fmt"
	"internal/buildcfg"
	"internal/testenv"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func linkedCoroStackUnsafe(t *testing.T, path string) bool {
	t.Helper()
	if f, err := elf.Open(path); err == nil {
		defer f.Close()
		symbols, err := f.Symbols()
		if err != nil {
			t.Fatal(err)
		}
		for _, symbol := range symbols {
			if symbol.Name != "runtime.coroStackUnsafe" {
				continue
			}
			section := f.Sections[symbol.Section]
			if section.Type == elf.SHT_NOBITS {
				return false
			}
			data, err := section.Data()
			if err != nil {
				t.Fatal(err)
			}
			return data[symbol.Value-section.Addr] != 0
		}
	} else if f, err := macho.Open(path); err == nil {
		defer f.Close()
		for _, symbol := range f.Symtab.Syms {
			if strings.TrimPrefix(symbol.Name, "_") != "runtime.coroStackUnsafe" {
				continue
			}
			for _, section := range f.Sections {
				if symbol.Value < section.Addr || symbol.Value >= section.Addr+section.Size {
					continue
				}
				if section.Flags&0xff == 1 {
					return false
				} // S_ZEROFILL
				data, err := section.Data()
				if err != nil {
					t.Fatal(err)
				}
				return data[symbol.Value-section.Addr] != 0
			}
		}
	}
	t.Fatalf("linked native-stack guard missing in %s", path)
	return false
}

func TestLinkedNativeStackBudget(t *testing.T) {
	if !buildcfg.Experiment.Coro {
		t.Skip("requires coroutine experiment")
	}
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" || runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native executor unavailable")
	}
	testenv.MustHaveGoBuild(t)
	goTool := testenv.GoToolPath(t)
	entry := `func entry(c chan uint64)uint64{v:=<-c;return leaf(v)}
func main(){c:=make(chan uint64,1);c<-1;if entry(c)!=2{panic("result")};println("linked-stack-ok")}`
	small := "package main\n//go:noinline\nfunc leaf(v uint64)uint64{return v+1}\n" + entry
	var deep strings.Builder
	deep.WriteString("package main\n")
	for i := 0; i < 64; i++ {
		fmt.Fprintf(&deep, "//go:noinline\nfunc f%d(v uint64)uint64{return f%d(v)+1}\n", i, i+1)
	}
	deep.WriteString("//go:noinline\nfunc f64(v uint64)uint64{return v}\nfunc leaf(v uint64)uint64{return f0(v)-63}\n" + entry)
	var large strings.Builder
	large.WriteString("package main\n//go:noinline\nfunc sum(")
	for i := 0; i < 160; i++ {
		if i != 0 {
			large.WriteString(",")
		}
		fmt.Fprintf(&large, "a%d uint64", i)
	}
	large.WriteString(")uint64{return ")
	for i := 0; i < 160; i++ {
		if i != 0 {
			large.WriteString("+")
		}
		fmt.Fprintf(&large, "a%d", i)
	}
	large.WriteString("}\nfunc entry(c chan uint64)uint64{v:=<-c;return sum(")
	for i := 0; i < 160; i++ {
		if i != 0 {
			large.WriteString(",")
		}
		large.WriteString("v")
	}
	large.WriteString(")}\nfunc main(){c:=make(chan uint64,1);c<-1;if entry(c)!=160{panic(\"result\")};println(\"linked-stack-ok\")}")
	indirect := `package main
//go:noinline
func leaf(v uint64)uint64{return v+1}
func entry(c chan uint64)uint64{f:=[]func(uint64)uint64{leaf,leaf}[len(c)%2];v:=<-c;return f(v)}
func main(){c:=make(chan uint64,1);c<-1;if entry(c)!=2{panic("result")};println("linked-stack-ok")}`
	for _, test := range []struct {
		name, source string
		unsafe       bool
	}{{"small", small, false}, {"deep", deep.String(), true}, {"late-spills", large.String(), true}, {"unproved-indirect", indirect, true}} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, "main.go")
			if err := os.WriteFile(source, []byte(test.source), 0600); err != nil {
				t.Fatal(err)
			}
			binary := filepath.Join(dir, "probe.exe")
			cmd := exec.Command(goTool, "build", "-gcflags=-l -d=coro=4", "-ldflags=-debugcorostack", "-o", binary, source)
			cmd.Env = append(os.Environ(), "GOENV=off", "GOTOOLCHAIN=local", "GOWORK=off", "GOEXPERIMENT=coro", "CGO_ENABLED=0")
			log, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("build: %v\n%s", err, log)
			}
			if got := linkedCoroStackUnsafe(t, binary); got != test.unsafe {
				t.Fatalf("unsafe=%t want=%t\n%s", got, test.unsafe, log)
			}
			if test.unsafe != strings.Contains(string(log), "coro-stack: ordinary fallback:") {
				t.Fatalf("incorrect proof diagnostic:\n%s", log)
			}
			for _, p := range []string{"1", "4"} {
				run := exec.Command(binary)
				run.Env = append(os.Environ(), "GOMAXPROCS="+p, "GOGC=1", "GODEBUG=asyncpreemptoff=1")
				out, err := run.CombinedOutput()
				if err != nil || strings.TrimSpace(string(out)) != "linked-stack-ok" {
					t.Fatalf("P%s: %v\n%s", p, err, out)
				}
			}
		})
	}
}
