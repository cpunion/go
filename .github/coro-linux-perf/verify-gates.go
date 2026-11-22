package main

import (
	"debug/elf"
	"debug/macho"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
)

func gate(data io.ReaderAt, offset uint64) (byte, error) {
	var value [1]byte
	_, err := data.ReadAt(value[:], int64(offset))
	return value[0], err
}

func unpack(path string) (map[string]byte, error) {
	out := make(map[string]byte)
	if exe, err := macho.Open(path); err == nil {
		defer exe.Close()
		for _, sym := range exe.Symtab.Syms {
			if !strings.HasSuffix(sym.Name, ".coro.stackgate") {
				continue
			}
			section := exe.Sections[int(sym.Sect)-1]
			b, err := gate(section, sym.Value-section.Addr)
			if err != nil {
				return nil, err
			}
			out[sym.Name] = b
		}
		return out, nil
	}
	exe, err := elf.Open(path)
	if err != nil {
		return nil, err
	}
	defer exe.Close()
	symbols, err := exe.Symbols()
	if err != nil {
		return nil, err
	}
	for _, sym := range symbols {
		if !strings.HasSuffix(sym.Name, ".coro.stackgate") {
			continue
		}
		section := exe.Sections[sym.Section]
		b, err := gate(section, sym.Value-section.Addr)
		if err != nil {
			return nil, err
		}
		out[sym.Name] = b
	}
	return out, nil
}

func main() {
	out, err := unpack(os.Args[1])
	if err != nil {
		log.Fatal(err)
	}
	for _, name := range os.Args[2:] {
		key := name + ".coro.stackgate"
		value, ok := out[key]
		if !ok || value != 0 {
			log.Fatalf("%s present=%t gate=%d", key, ok, value)
		}
		fmt.Println(key, value)
	}
}
