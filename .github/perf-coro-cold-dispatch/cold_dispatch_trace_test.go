//go:build corocoldtrace

package corobench

import (
	"os"
	"testing"
	_ "unsafe"
)

//go:linkname dumpStacklessCoroColdTrace runtime.dumpStacklessCoroColdTrace
func dumpStacklessCoroColdTrace()

func TestMain(m *testing.M) {
	code := m.Run()
	dumpStacklessCoroColdTrace()
	os.Exit(code)
}
