// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This implements the write barrier buffer. The write barrier itself
// is gcWriteBarrier and is implemented in assembly.
//
// See mbarrier.go for algorithmic details on the write barrier. This
// file deals only with the buffer.
//
// The write barrier has a fast path and a slow path. The fast path
// simply enqueues to a per-P write barrier buffer. It's written in
// assembly and doesn't clobber any general purpose registers, so it
// doesn't have the usual overheads of a Go call.
//
// When the buffer fills up, the write barrier invokes the slow path
// (wbBufFlush) to flush the buffer to the GC work queues. In this
// path, since the compiler didn't spill registers, we spill *all*
// registers and disallow any GC safe points that could observe the
// stack frame (since we don't know the types of the spilled
// registers).

package runtime

import (
	"internal/goarch"
	"runtime/internal/atomic"
	"unsafe"
)

// testSmallBuf forces a small write barrier buffer to stress write
// barrier flushing.
const testSmallBuf = false

// wbBuf is a per-P buffer of pointers queued by the write barrier.
// This buffer is flushed to the GC workbufs when it fills up and on
// various GC transitions.
//
// This is closely related to a "sequential store buffer" (SSB),
// except that SSBs are usually used for maintaining remembered sets,
// while this is used for marking.
type wbBuf struct {
	// next points to the next slot in buf. It must not be a
	// pointer type because it can point past the end of buf and
	// must be updated without write barriers.
	//
	// This is a pointer rather than an index to optimize the
	// write barrier assembly.
	next uintptr

	// end points to just past the end of buf. It must not be a
	// pointer type because it points past the end of buf and must
	// be updated without write barriers.
	end uintptr

	// buf stores a series of pointers to execute write barriers
	// on. This must be a multiple of wbBufEntryPointers because
	// the write barrier only checks for overflow once per entry.
	buf [wbBufEntryPointers * wbBufEntries]uintptr
}

const (
	// wbBufEntries is the number of write barriers between
	// flushes of the write barrier buffer.
	//
	// This trades latency for throughput amortization. Higher
	// values amortize flushing overhead more, but increase the
	// latency of flushing. Higher values also increase the cache
	// footprint of the buffer.
	//
	// TODO: What is the latency cost of this? Tune this value.
	wbBufEntries = 256

	// wbBufEntryPointers is the number of pointers added to the
	// buffer by each write barrier.
	wbBufEntryPointers = 2
)

// reset empties b by resetting its next and end pointers.
func (b *wbBuf) reset() {
	start := uintptr(unsafe.Pointer(&b.buf[0]))
	b.next = start
	if testSmallBuf {
		// For testing, allow two barriers in the buffer. If
		// we only did one, then barriers of non-heap pointers
		// would be no-ops. This lets us combine a buffered
		// barrier with a flush at a later time.
		b.end = uintptr(unsafe.Pointer(&b.buf[2*wbBufEntryPointers]))
	} else {
		b.end = start + uintptr(len(b.buf))*unsafe.Sizeof(b.buf[0])
	}

	if (b.end-b.next)%(wbBufEntryPointers*unsafe.Sizeof(b.buf[0])) != 0 {
		throw("bad write barrier buffer bounds")
	}
}

// discard resets b's next pointer, but not its end pointer.
//
// This must be nosplit because it's called by wbBufFlush.
//
//go:nosplit
func (b *wbBuf) discard() {
	b.next = uintptr(unsafe.Pointer(&b.buf[0]))
}

// empty reports whether b contains no pointers.
func (b *wbBuf) empty() bool {
	return b.next == uintptr(unsafe.Pointer(&b.buf[0]))
}

// putFast adds old and new to the write barrier buffer and returns
// false if a flush is necessary. Callers should use this as:
//
//	buf := &getg().m.p.ptr().wbBuf
//	if !buf.putFast(old, new) {
//	    wbBufFlush()
//	}
//	... actual memory write ...
//
// The caller must ensure there are no preemption points during the
// above sequence. There must be no preemption points while buf is in
// use because it is a per-P resource. There must be no preemption
// points between the buffer put and the write to memory because this
// could allow a GC phase change, which could result in missed write
// barriers.
//
// putFast must be nowritebarrierrec to because write barriers here would
// corrupt the write barrier buffer. It (and everything it calls, if
// it called anything) has to be nosplit to avoid scheduling on to a
// different P and a different buffer.
//
//go:nowritebarrierrec
//go:nosplit
func (b *wbBuf) putFast(old, new uintptr) bool {
	p := (*[2]uintptr)(unsafe.Pointer(b.next))
	p[0] = old
	p[1] = new
	b.next += 2 * goarch.PtrSize
	return b.next != b.end
}

// wbBufFlush flushes the current P's write barrier buffer to the GC
// workbufs.
//
// This must not have write barriers because it is part of the write
// barrier implementation.
//
// This and everything it calls must be nosplit because 1) the stack
// contains untyped slots from gcWriteBarrier and 2) there must not be
// a GC safe point between the write barrier test in the caller and
// flushing the buffer.
//
// TODO: A "go:nosplitrec" annotation would be perfect for this.
//
//go:nowritebarrierrec
//go:nosplit
func wbBufFlush() {
	// Note: Every possible return from this function must reset
	// the buffer's next pointer to prevent buffer overflow.

	// This *must not* modify its arguments because this
	// function's argument slots do double duty in gcWriteBarrier
	// as register spill slots. Currently, not modifying the
	// arguments is sufficient to keep the spill slots unmodified
	// (which seems unlikely to change since it costs little and
	// helps with debugging).

	if getg().m.dying > 0 {
		// We're going down. Not much point in write barriers
		// and this way we can allow write barriers in the
		// panic path.
		getg().m.p.ptr().wbBuf.discard()
		return
	}

	// Switch to the system stack so we don't have to worry about
	// the untyped stack slots or safe points.
	systemstack(func() {
		wbBufFlush1(getg().m.p.ptr())
	})
}

// wbBufFlush1 flushes p's write barrier buffer to the GC work queue.
//
// This must not have write barriers because it is part of the write
// barrier implementation, so this may lead to infinite loops or
// buffer corruption.
//
// This must be non-preemptible because it uses the P's workbuf.
//
//go:nowritebarrierrec
//go:systemstack
func wbBufFlush1(pp *p) {
	// Get the buffered pointers.
	start := uintptr(unsafe.Pointer(&pp.wbBuf.buf[0]))
	n := (pp.wbBuf.next - start) / unsafe.Sizeof(pp.wbBuf.buf[0])
	ptrs := pp.wbBuf.buf[:n]

	// Poison the buffer to make extra sure nothing is enqueued
	// while we're processing the buffer.
	pp.wbBuf.next = 0

	if useCheckmark {
		// Slow path for checkmark mode.
		for _, ptr := range ptrs {
			shade(ptr)
		}
		pp.wbBuf.reset()
		return
	}

	// Mark all of the pointers in the buffer and record only the
	// pointers we greyed. We use the buffer itself to temporarily
	// record greyed pointers.
	//
	// TODO: Should scanobject/scanblock just stuff pointers into
	// the wbBuf? Then this would become the sole greying path.
	//
	// TODO: We could avoid shading any of the "new" pointers in
	// the buffer if the stack has been shaded, or even avoid
	// putting them in the buffer at all (which would double its
	// capacity). This is slightly complicated with the buffer; we
	// could track whether any un-shaded goroutine has used the
	// buffer, or just track globally whether there are any
	// un-shaded stacks and flush after each stack scan.
	gcw := &pp.gcw
	pos := 0
	for _, ptr := range ptrs {
		if ptr < minLegalPointer {
			// nil pointers are very common, especially
			// for the "old" values. Filter out these and
			// other "obvious" non-heap pointers ASAP.
			//
			// TODO: Should we filter out nils in the fast
			// path to reduce the rate of flushes?
			continue
		}
		obj, span, objIndex := findObject(ptr, 0, 0)
		if obj == 0 {
			continue
		}
		// TODO: Consider making two passes where the first
		// just prefetches the mark bits.
		mbits := span.markBitsForIndex(objIndex)
		if mbits.isMarked() {
			continue
		}
		mbits.setMarked()

		// Mark span.
		arena, pageIdx, pageMask := pageIndexOf(span.base())
		if arena.pageMarks[pageIdx]&pageMask == 0 {
			atomic.Or8(&arena.pageMarks[pageIdx], pageMask)
		}

		if span.spanclass.noscan() {
			gcw.bytesMarked += uint64(span.elemsize)
			continue
		}
		ptrs[pos] = obj
		pos++
	}

	// Enqueue the greyed objects.
	gcw.putBatch(ptrs[:pos])

	pp.wbBuf.reset()
}
