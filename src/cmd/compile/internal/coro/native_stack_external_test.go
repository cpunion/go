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

func linkedCoroStackUnsafe(t *testing.T, path, name string) bool {
	t.Helper()
	if f, err := elf.Open(path); err == nil {
		defer f.Close()
		symbols, err := f.Symbols()
		if err != nil {
			t.Fatal(err)
		}
		for _, symbol := range symbols {
			if symbol.Name != name {
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
			if strings.TrimPrefix(symbol.Name, "_") != name {
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
	t.Fatalf("linked native-stack guard %s missing in %s", name, path)
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
func other(v uint64)uint64{return v+1}
func entry(c chan uint64)uint64{f:=leaf;if len(c)==0 {f=other};v:=<-c;return f(v)}
func main(){c:=make(chan uint64,1);c<-1;if entry(c)!=2{panic("result")};println("linked-stack-ok")}`
	// The original function-value call now crosses a managed boundary. Keep
	// a separate raw indirect cleanup in a plain callee to test rejection.
	unprovedIndirect := `package main
var cleanupCount int
//go:noinline
func first(){cleanupCount++}
//go:noinline
func second(){cleanupCount+=2}
//go:noinline
func leaf(v uint64)uint64{f:=first;if v==0{f=second};defer f();return v+1}
func entry(c chan uint64)uint64{v:=<-c;return leaf(v)}
func main(){c:=make(chan uint64,1);c<-1;if entry(c)!=2||cleanupCount!=1{panic("result")};println("linked-stack-ok")}
`
	for _, test := range []struct {
		name, source string
		unsafe       bool
	}{{"small", small, false}, {"deep", deep.String(), true}, {"native-child", strings.Replace(deep.String(), "func entry(c chan uint64)uint64", "func child(c chan uint64)uint64", 1) + "\nfunc entry(c chan uint64)uint64{return child(c)}", true}, {"late-spills", large.String(), true}, {"managed-indirect", indirect, false}, {"unproved-indirect", unprovedIndirect, true}} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, "main.go")
			isolated := strings.Replace(test.source, "func main(){", "func safe(c chan uint64)uint64{return <-c+1}\nfunc main(){z:=make(chan uint64,1);z<-8;if safe(z)!=9{panic(\"safe\")};", 1)
			if err := os.WriteFile(source, []byte(isolated), 0600); err != nil {
				t.Fatal(err)
			}
			binary := filepath.Join(dir, "probe.exe")
			cmd := exec.Command(goTool, "build", "-gcflags=-l -d=coro=4", "-ldflags=-debugcorostack", "-o", binary, source)
			cmd.Env = append(os.Environ(), "GOENV=off", "GOTOOLCHAIN=local", "GOWORK=off", "GOEXPERIMENT=coro", "CGO_ENABLED=0")
			log, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("build: %v\n%s", err, log)
			}
			if got := linkedCoroStackUnsafe(t, binary, "main.entry.coro.stackgate"); got != test.unsafe {
				t.Fatalf("unsafe=%t want=%t\n%s", got, test.unsafe, log)
			}
			if test.name == "managed-indirect" {
				inspect := exec.Command(goTool, "tool", "objdump", "-s", "^main[.]entry[.]coro[.]func[0-9]+$", binary)
				code, err := inspect.CombinedOutput()
				if err != nil || !strings.Contains(string(code), "runtime.coroHostCall") {
					t.Fatalf("managed indirect call lost ordinary-host handoff: %v\n%s", err, code)
				}
			}
			if test.name == "unproved-indirect" && !strings.Contains(string(log), "(at main.leaf)") {
				t.Fatalf("raw indirect cleanup lost its linked proof failure:\n%s", log)
			}
			if linkedCoroStackUnsafe(t, binary, "main.safe.coro.stackgate") {
				t.Fatalf("unrelated safe entry fell back:\n%s", log)
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

// An imported cleanup is not compiled by its caller's backend. Its actual
// linked code must still be checked, and sharing a safe cleanup must not tie
// the two callers' eligibility together.
func TestLinkedNativeStackImportedCleanup(t *testing.T) {
	if !buildcfg.Experiment.Coro {
		t.Skip("requires coroutine experiment")
	}
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" || runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native executor unavailable")
	}
	testenv.MustHaveGoBuild(t)
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "helper"), 0700); err != nil {
		t.Fatal(err)
	}
	var helper strings.Builder
	helper.WriteString("package helper\nvar Calls,Value int\nvar Seen any\n//go:noinline\nfunc Shared(){Calls++}\n//go:noinline\nfunc Deep(){Value=f0(1);Seen=recover()}\n")
	for i := 0; i < 64; i++ {
		fmt.Fprintf(&helper, "//go:noinline\nfunc f%d(v int)int{return f%d(v)+1}\n", i, i+1)
	}
	helper.WriteString("//go:noinline\nfunc f64(v int)int{return v}\n")
	for name, text := range map[string]string{
		"go.mod":           "module example.com/cleanup\ngo 1.27\n",
		"helper/helper.go": helper.String(),
		"main.go": `package main
import "example.com/cleanup/helper"
func entry(c chan int)int {defer helper.Shared();defer helper.Deep();return <-c+1}
func safe(c chan int)int {defer helper.Shared();return <-c+1}
func main(){c:=make(chan int,1);c<-1;if entry(c)!=2{panic("entry")};c<-8;if safe(c)!=9{panic("safe")};if helper.Value!=65||helper.Calls!=2{panic("cleanup")};println("imported-cleanup-ok")}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	binary := filepath.Join(dir, "probe.exe")
	cmd := exec.Command(testenv.GoToolPath(t), "build", "-gcflags=-l -d=coro=4", "-gcflags=example.com/cleanup/helper=-l -d=coro=4", "-ldflags=-debugcorostack", "-o", binary, ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOENV=off", "GOTOOLCHAIN=local", "GOWORK=off", "GOEXPERIMENT=coro", "CGO_ENABLED=0")
	log, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build: %v\n%s", err, log)
	}
	if !linkedCoroStackUnsafe(t, binary, "main.entry.coro.stackgate") || linkedCoroStackUnsafe(t, binary, "main.safe.coro.stackgate") {
		t.Fatalf("incorrect imported cleanup gates:\n%s", log)
	}
	for _, p := range []string{"1", "4"} {
		run := exec.Command(binary)
		run.Env = append(os.Environ(), "GOMAXPROCS="+p, "GOGC=1")
		out, err := run.CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != "imported-cleanup-ok" {
			t.Fatalf("P%s: %v\n%s", p, err, out)
		}
	}
}

func TestLinkedNativeStackParallelMetadata(t *testing.T) {
	if !buildcfg.Experiment.Coro {
		t.Skip("requires coroutine experiment")
	}
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" || runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native executor unavailable")
	}
	testenv.MustHaveGoBuild(t)
	var source strings.Builder
	source.WriteString("package main\n")
	for i := 0; i < 128; i++ {
		fmt.Fprintf(&source, "//go:noinline\nfunc f%d(c chan int)int{return <-c+1}\n", i)
	}
	source.WriteString("func main(){c:=make(chan int,1);v:=0;")
	for i := 0; i < 128; i++ {
		fmt.Fprintf(&source, "c<-1;v+=f%d(c);", i)
	}
	source.WriteString("if v!=256{panic(\"result\")};println(\"parallel-metadata-ok\")}")
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	if err := os.WriteFile(path, []byte(source.String()), 0600); err != nil {
		t.Fatal(err)
	}
	for _, workers := range []string{"2", "4", "8"} {
		binary := filepath.Join(dir, "parallel-"+workers+".exe")
		cmd := exec.Command(testenv.GoToolPath(t), "build", "-gcflags=-l -c="+workers+" -d=coro=4", "-o", binary, path)
		cmd.Env = append(os.Environ(), "GOENV=off", "GOTOOLCHAIN=local", "GOWORK=off", "GOEXPERIMENT=coro", "CGO_ENABLED=0", "GOMAXPROCS=4")
		log, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("workers %s: %v\n%s", workers, err, log)
		}
		for _, name := range []string{"main.f0", "main.f63", "main.f127"} {
			if linkedCoroStackUnsafe(t, binary, name+".coro.stackgate") {
				t.Fatalf("workers %s: safe %s fell back", workers, name)
			}
		}
		for _, p := range []string{"1", "4"} {
			run := exec.Command(binary)
			run.Env = append(os.Environ(), "GOMAXPROCS="+p, "GOGC=1")
			out, err := run.CombinedOutput()
			if err != nil || strings.TrimSpace(string(out)) != "parallel-metadata-ok" {
				t.Fatalf("workers %s P%s: %v\n%s", workers, p, err, out)
			}
		}
	}
}

func TestLinkedNativeStackPlugin(t *testing.T) {
	if !buildcfg.Experiment.Coro {
		t.Skip("requires coroutine experiment")
	}
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" || runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native executor unavailable")
	}
	testenv.MustHaveGoBuild(t)
	testenv.MustHaveCGO(t)
	testenv.MustHaveBuildMode(t, "plugin")
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "payload"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "host"), 0700); err != nil {
		t.Fatal(err)
	}
	var payload strings.Builder
	payload.WriteString("package main\nfunc Evaluate()uint64{c:=make(chan uint64,1);c<-1;return root(c)}\n//go:noinline\nfunc root(c chan uint64)uint64{v:=<-c;return f0(v)}\n")
	for i := 0; i < 4096; i++ {
		fmt.Fprintf(&payload, "//go:noinline\nfunc f%d(v uint64)uint64{return f%d(v)+1}\n", i, i+1)
	}
	payload.WriteString("//go:noinline\nfunc f4096(v uint64)uint64{return v}\n")
	for name, text := range map[string]string{
		"go.mod":             "module example.com/stackplugin\ngo 1.27\n",
		"payload/payload.go": payload.String(),
		"host/main.go": `package main
import("os";"plugin";"runtime")
//go:noinline
func hostEntry(c chan uint64)uint64{return <-c+1}
func check(){c:=make(chan uint64,1);c<-8;if hostEntry(c)!=9{panic("host")}}
func main(){check();p,e:=plugin.Open(os.Args[1]);if e!=nil{panic(e)};v,e:=p.Lookup("Evaluate");if e!=nil{panic(e)};f:=v.(func()uint64);for range 4{runtime.GC();if f()!=4097{panic("plugin")};check()};println("module-stack-ok")}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	goTool := testenv.GoToolPath(t)
	pluginPath := filepath.Join(dir, "payload.so")
	host := filepath.Join(dir, "host.exe")
	for _, target := range []struct {
		output, pkg string
		flags       []string
	}{
		{pluginPath, "./payload", []string{"-buildmode=plugin"}},
		{host, "./host", nil},
	} {
		args := append([]string{"build", "-gcflags=-l -d=coro=4", "-ldflags=-debugcorostack", "-o", target.output}, target.flags...)
		args = append(args, target.pkg)
		cmd := exec.Command(goTool, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOENV=off", "GOTOOLCHAIN=local", "GOWORK=off", "GOEXPERIMENT=coro", "CGO_ENABLED=1")
		if log, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", target.pkg, err, log)
		}
	}
	if !linkedCoroStackUnsafe(t, host, "main.hostEntry.coro.stackgate") {
		t.Fatal("plugin-capable host retained native entry")
	}
	cmd := exec.Command(goTool, "tool", "objdump", "-s", `example\.com/stackplugin/payload\.root$`, pluginPath)
	log, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(log), "payload.root") || strings.Contains(string(log), "runtime.coroRun") {
		t.Fatalf("module root lost ordinary source body: %v\n%s", err, log)
	}
	for _, p := range []string{"1", "4"} {
		run := exec.Command(host, pluginPath)
		run.Env = append(os.Environ(), "GOMAXPROCS="+p, "GOGC=1")
		out, err := run.CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != "module-stack-ok" {
			t.Fatalf("P%s: %v\n%s", p, err, out)
		}
	}
}
