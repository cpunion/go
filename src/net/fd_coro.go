// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package net

import (
	"internal/poll"
	"io"
	"runtime"
	"syscall"
	"unsafe"
)

// coroReadStart is the compiler-private entry used for a Read suspension.
// The common path borrows the connection's poll descriptor while holding its
// read lock. Lock contention alone uses the bounded blocking-read worker pool.
func (c *conn) coroReadStart(ctx unsafe.Pointer, b []byte, n *int, err *error, direct *bool, status *uintptr) bool {
	*n = 0
	*err = nil
	*direct = false
	*status = 0
	if !c.ok() {
		*err = syscall.EINVAL
		return false
	}

	state, e := c.fd.pfd.CoroReadStart(ctx, b, n, status)
	if state == poll.CoroReadFallback {
		poll.CoroCallRead(ctx, func() {
			count, readErr := c.fd.Read(b)
			*n = count
			*err = c.coroReadError(readErr)
		})
		return true
	}
	if state == poll.CoroReadWait {
		*direct = true
		return true
	}
	runtime.KeepAlive(c.fd)
	*err = c.coroReadError(wrapSyscallError(readSyscallName, e))
	return false
}

// coroReadFinish restores the public Read result after a direct coroutine
// operation. A fallback worker has already written the final result.
func (c *conn) coroReadFinish(n *int, err *error, direct bool, status uintptr) {
	if !direct {
		return
	}
	e := c.fd.pfd.CoroReadFinish(n, status)
	runtime.KeepAlive(c.fd)
	*err = c.coroReadError(wrapSyscallError(readSyscallName, e))
}

func (c *conn) coroReadError(err error) error {
	if err != nil && err != io.EOF {
		return &OpError{Op: "read", Net: c.fd.net, Source: c.fd.laddr, Addr: c.fd.raddr, Err: err}
	}
	return err
}

func (fd *netFD) coroAcceptStep(ctx unsafe.Pointer, held *bool) (*netFD, error, bool) {
	d, rsa, errcall, err, parked := fd.pfd.CoroAcceptStep(ctx, held)
	if parked {
		return nil, nil, true
	}
	if err != nil {
		if errcall != "" {
			err = wrapSyscallError(errcall, err)
		}
		return nil, err, false
	}
	netfd := newFD(d, fd.family, fd.sotype, fd.net)
	if err = netfd.init(); err != nil {
		netfd.Close()
		return nil, err, false
	}
	lsa, _ := syscall.Getsockname(netfd.pfd.Sysfd)
	netfd.setAddr(netfd.addrFunc()(lsa), netfd.addrFunc()(rsa))
	return netfd, nil, false
}

// coroAcceptTCPStep is the compiler-private non-blocking accept step for *TCPListener.
func (l *TCPListener) coroAcceptTCPStep(ctx unsafe.Pointer, out **TCPConn, errOut *error, held *bool) bool {
	if !l.ok() {
		*out = nil
		*errOut = syscall.EINVAL
		return false
	}
	fd, err, parked := l.fd.coroAcceptStep(ctx, held)
	if parked {
		return true
	}
	if err != nil {
		*out = nil
		*errOut = &OpError{Op: "accept", Net: l.fd.net, Source: nil, Addr: l.fd.laddr, Err: err}
		return false
	}
	*out = newTCPConn(fd, l.lc.KeepAlive, l.lc.KeepAliveConfig, testPreHookSetKeepAlive, testHookSetKeepAlive)
	*errOut = nil
	return false
}

// coroAcceptStep is the compiler-private non-blocking Listener.Accept step for *TCPListener.
func (l *TCPListener) coroAcceptStep(ctx unsafe.Pointer, out *Conn, errOut *error, held *bool) bool {
	var tc *TCPConn
	if l.coroAcceptTCPStep(ctx, &tc, errOut, held) {
		return true
	}
	if *errOut != nil {
		*out = nil
		return false
	}
	*out = tc
	return false
}

// coroAcceptUnixStep is the compiler-private non-blocking accept step for *UnixListener.
func (l *UnixListener) coroAcceptUnixStep(ctx unsafe.Pointer, out **UnixConn, errOut *error, held *bool) bool {
	if !l.ok() {
		*out = nil
		*errOut = syscall.EINVAL
		return false
	}
	fd, err, parked := l.fd.coroAcceptStep(ctx, held)
	if parked {
		return true
	}
	if err != nil {
		*out = nil
		*errOut = &OpError{Op: "accept", Net: l.fd.net, Source: nil, Addr: l.fd.laddr, Err: err}
		return false
	}
	*out = newUnixConn(fd)
	*errOut = nil
	return false
}

// coroAcceptStep is the compiler-private non-blocking Listener.Accept step for *UnixListener.
func (l *UnixListener) coroAcceptStep(ctx unsafe.Pointer, out *Conn, errOut *error, held *bool) bool {
	var uc *UnixConn
	if l.coroAcceptUnixStep(ctx, &uc, errOut, held) {
		return true
	}
	if *errOut != nil {
		*out = nil
		return false
	}
	*out = uc
	return false
}

func runtime_coroRegisterIO(methodFn, startFn, finishFn any)
func runtime_coroRegisterAccept(methodFn, stepFn any)

func coroConnReadStart(raw, ctx unsafe.Pointer, b []byte, n *int, err *error, direct *bool, status *uintptr) bool {
	return (*conn)(raw).coroReadStart(ctx, b, n, err, direct, status)
}
func coroConnReadFinish(raw unsafe.Pointer, n *int, err *error, direct bool, status uintptr) {
	(*conn)(raw).coroReadFinish(n, err, direct, status)
}
func coroConnWriteStart(raw, ctx unsafe.Pointer, b []byte, n *int, err *error, direct *bool, status *uintptr) bool {
	return (*conn)(raw).coroWriteStart(ctx, b, n, err, direct, status)
}
func coroConnWriteFinish(raw unsafe.Pointer, n *int, err *error, direct bool, status uintptr) {
	(*conn)(raw).coroWriteFinish(n, err, direct, status)
}

func coroTCPConnReadStart(raw, ctx unsafe.Pointer, b []byte, n *int, err *error, direct *bool, status *uintptr) bool {
	return (*TCPConn)(raw).coroReadStart(ctx, b, n, err, direct, status)
}
func coroTCPConnReadFinish(raw unsafe.Pointer, n *int, err *error, direct bool, status uintptr) {
	(*TCPConn)(raw).coroReadFinish(n, err, direct, status)
}
func coroTCPConnWriteStart(raw, ctx unsafe.Pointer, b []byte, n *int, err *error, direct *bool, status *uintptr) bool {
	return (*TCPConn)(raw).coroWriteStart(ctx, b, n, err, direct, status)
}
func coroTCPConnWriteFinish(raw unsafe.Pointer, n *int, err *error, direct bool, status uintptr) {
	(*TCPConn)(raw).coroWriteFinish(n, err, direct, status)
}

func coroUnixConnReadStart(raw, ctx unsafe.Pointer, b []byte, n *int, err *error, direct *bool, status *uintptr) bool {
	return (*UnixConn)(raw).coroReadStart(ctx, b, n, err, direct, status)
}
func coroUnixConnReadFinish(raw unsafe.Pointer, n *int, err *error, direct bool, status uintptr) {
	(*UnixConn)(raw).coroReadFinish(n, err, direct, status)
}
func coroUnixConnWriteStart(raw, ctx unsafe.Pointer, b []byte, n *int, err *error, direct *bool, status *uintptr) bool {
	return (*UnixConn)(raw).coroWriteStart(ctx, b, n, err, direct, status)
}
func coroUnixConnWriteFinish(raw unsafe.Pointer, n *int, err *error, direct bool, status uintptr) {
	(*UnixConn)(raw).coroWriteFinish(n, err, direct, status)
}

func coroUDPConnReadStart(raw, ctx unsafe.Pointer, b []byte, n *int, err *error, direct *bool, status *uintptr) bool {
	return (*UDPConn)(raw).coroReadStart(ctx, b, n, err, direct, status)
}
func coroUDPConnReadFinish(raw unsafe.Pointer, n *int, err *error, direct bool, status uintptr) {
	(*UDPConn)(raw).coroReadFinish(n, err, direct, status)
}
func coroUDPConnWriteStart(raw, ctx unsafe.Pointer, b []byte, n *int, err *error, direct *bool, status *uintptr) bool {
	return (*UDPConn)(raw).coroWriteStart(ctx, b, n, err, direct, status)
}
func coroUDPConnWriteFinish(raw unsafe.Pointer, n *int, err *error, direct bool, status uintptr) {
	(*UDPConn)(raw).coroWriteFinish(n, err, direct, status)
}

func coroIPConnReadStart(raw, ctx unsafe.Pointer, b []byte, n *int, err *error, direct *bool, status *uintptr) bool {
	return (*IPConn)(raw).coroReadStart(ctx, b, n, err, direct, status)
}
func coroIPConnReadFinish(raw unsafe.Pointer, n *int, err *error, direct bool, status uintptr) {
	(*IPConn)(raw).coroReadFinish(n, err, direct, status)
}
func coroIPConnWriteStart(raw, ctx unsafe.Pointer, b []byte, n *int, err *error, direct *bool, status *uintptr) bool {
	return (*IPConn)(raw).coroWriteStart(ctx, b, n, err, direct, status)
}
func coroIPConnWriteFinish(raw unsafe.Pointer, n *int, err *error, direct bool, status uintptr) {
	(*IPConn)(raw).coroWriteFinish(n, err, direct, status)
}

func coroTCPAcceptStepAdapter(raw, ctx unsafe.Pointer, out *Conn, errOut *error, held *bool) bool {
	return (*TCPListener)(raw).coroAcceptStep(ctx, out, errOut, held)
}

func coroUnixAcceptStepAdapter(raw, ctx unsafe.Pointer, out *Conn, errOut *error, held *bool) bool {
	return (*UnixListener)(raw).coroAcceptStep(ctx, out, errOut, held)
}

func init() {
	runtime_coroRegisterIO((*conn).Read, coroConnReadStart, coroConnReadFinish)
	runtime_coroRegisterIO((*conn).Write, coroConnWriteStart, coroConnWriteFinish)
	runtime_coroRegisterIO((*TCPConn).Read, coroTCPConnReadStart, coroTCPConnReadFinish)
	runtime_coroRegisterIO((*TCPConn).Write, coroTCPConnWriteStart, coroTCPConnWriteFinish)
	runtime_coroRegisterIO((*UnixConn).Read, coroUnixConnReadStart, coroUnixConnReadFinish)
	runtime_coroRegisterIO((*UnixConn).Write, coroUnixConnWriteStart, coroUnixConnWriteFinish)
	runtime_coroRegisterIO((*UDPConn).Read, coroUDPConnReadStart, coroUDPConnReadFinish)
	runtime_coroRegisterIO((*UDPConn).Write, coroUDPConnWriteStart, coroUDPConnWriteFinish)
	runtime_coroRegisterIO((*IPConn).Read, coroIPConnReadStart, coroIPConnReadFinish)
	runtime_coroRegisterIO((*IPConn).Write, coroIPConnWriteStart, coroIPConnWriteFinish)
	runtime_coroRegisterAccept((*TCPListener).Accept, coroTCPAcceptStepAdapter)
	runtime_coroRegisterAccept((*UnixListener).Accept, coroUnixAcceptStepAdapter)
}
