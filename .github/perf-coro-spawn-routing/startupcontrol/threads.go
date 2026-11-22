package startupcontrol

import (
	"runtime"
	"sync"
)

// PrimeThreads makes each helper enter an OS thread before releasing the group.
func PrimeThreads() {
	procs := runtime.GOMAXPROCS(0)
	var ready, done sync.WaitGroup
	release := make(chan struct{})
	ready.Add(procs)
	done.Add(procs)
	for range procs {
		go func() {
			runtime.LockOSThread()
			ready.Done()
			<-release
			runtime.UnlockOSThread()
			done.Done()
		}()
	}
	ready.Wait()
	close(release)
	done.Wait()
	for range procs {
		runtime.Gosched()
	}
}
