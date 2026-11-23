// Copyright (c) 2015 Joseph D Poirier
// Distributable under the terms of The New BSD License
// that can be found in the LICENSE file.

// Package godump978 wraps libdump978, a 978MHz UAT demodulator.

package godump978

import (
	"encoding/hex"
	"strconv"
	"strings"
	"sync/atomic"
	"unsafe"
)

/*
#include <stdint.h>
#include "../dump978/dump978.h"
*/
import "C"

// OutChan is a buffered output channel for demodulated data.
var OutChan = make(chan string, 100)

// DroppedFrames counts demodulated frames dropped because OutChan was full.
var DroppedFrames atomic.Uint64

//export dump978Cb
func dump978Cb(updown C.char, data *C.uint8_t, length C.int, rs_errors C.int, signal_strength C.int) {
	if length < 0 || data == nil {
		return
	}

	// c buffer to go slice without copying
	buf := unsafe.Slice((*byte)(unsafe.Pointer(data)), int(length))

	// copy incoming to outgoing
	var sb strings.Builder
	sb.Grow(1 + len(buf)*2 + 32)
	sb.WriteByte(byte(updown))
	sb.WriteString(hex.EncodeToString(buf))
	sb.WriteString(";rs=")
	sb.WriteString(strconv.Itoa(int(rs_errors)))
	sb.WriteString(";ss=")
	sb.WriteString(strconv.Itoa(int(signal_strength)))
	sb.WriteByte(';')

	select {
	case OutChan <- sb.String():
	default:
		DroppedFrames.Add(1)
	}
}
