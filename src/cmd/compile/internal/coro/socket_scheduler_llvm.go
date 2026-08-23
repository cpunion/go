// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

// renderSocketObjectLLVMForTarget emits the connected-socket counterpart of
// the blocking-file vertical slice. The calling thread remains in recv while
// a replacement native thread owns the scheduler queue and advances a sibling
// coroutine. The sibling sends the byte that releases recv.
func renderSocketObjectLLVMForTarget(hostName string, value int64, goos, goarch string) ([]byte, error) {
	return renderBlockingObjectLLVMForTarget(hostName, value, goos, goarch, blockingObjectLLVMRecipe{
		namespace: "socket",
		declarations: `declare i32 @socketpair(i32, i32, i32, ptr)
declare i64 @recv(i32, ptr, i64, i32)
declare i64 @send(i32, ptr, i64, i32)`,
		// AF_UNIX and SOCK_STREAM are both 1 on the supported Darwin and
		// Linux targets. A connected pair isolates scheduler ownership and
		// blocking socket behavior from listener setup and DNS.
		initialize: `  %socket.status = call i32 @socketpair(i32 1, i32 1, i32 0, ptr %fds.ptr)
  %socket.ok = icmp eq i32 %socket.status, 0
  br i1 %socket.ok, label %initialize.io, label %fail.free`,
		read:  "call i64 @recv(i32 %read.fd, ptr %read.byte.ptr, i64 1, i32 0)",
		write: "call i64 @send(i32 %write.fd, ptr %write.byte.ptr, i64 1, i32 0)",
	})
}
