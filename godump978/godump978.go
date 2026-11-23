// Copyright (c) 2015 Joseph D Poirier
// Distributable under the terms of The New BSD License
// that can be found in the LICENSE file.

// Package godump978 wraps libdump978, a 978MHz UAT demodulator.
//
// Build example
//
// dump978.so:
//   $ gcc -c -O2 -g -Wall -Werror -Ifec -fpic -DBUILD_LIB=1 dump978.c fec.c fec/decode_rs_char.c fec/init_rs_char.c
//   $ gcc -shared -lm -o ../libdump978.so dump978.o fec.o decode_rs_char.o init_rs_char.o
//
// dump978 go wrapper:
//   $ go build -o dump978.a dump978.go dump978_exports.go
//
// uat_read executable:
//   $ go build uat_read.go

package godump978

/*
#cgo linux LDFLAGS: -L${SRCDIR}/.. -ldump978 -lm
#cgo darwin LDFLAGS: -L${SRCDIR}/.. -ldump978 -lm
#cgo windows CFLAGS: -IC:/WINDOWS/system32
#cgo windows LDFLAGS: -L. -ldump978 -LC:/WINDOWS/system32

#include <stdlib.h>
#include <stdint.h>
#include "../dump978/dump978.h"

extern void dump978Cb(char updown, uint8_t *data, int len, int rs_errors, int signal_strength);
static inline CallBack GetGoCb() {
	return (CallBack)dump978Cb;
}
*/
import "C"
import (
	"math"
	"sync"
	"unsafe"
)

// InChan is a buffered input channel for raw data.
var InChan = make(chan []byte, 100)

var processMu sync.Mutex

func init() {
	C.Dump978Init((C.CallBack)(C.GetGoCb()))
}

// ProcessData passes buf (modulated data) to dump978 for demodulation.
func ProcessData(buf []byte) {
	if len(buf) == 0 {
		return
	}
	processMu.Lock()
	defer processMu.Unlock()
	const maxChunk = math.MaxInt32 &^ 1
	for len(buf) > 0 {
		chunk := len(buf)
		if chunk > maxChunk {
			chunk = maxChunk
		}
		C.process_data((*C.char)(unsafe.Pointer(&buf[0])), C.int(chunk))
		buf = buf[chunk:]
	}
}

// IQStats holds RF IQ signal-health telemetry collected from raw 8-bit RTL-SDR samples.
type IQStats struct {
	TotalSamples   uint64
	ClippedSamples uint64
	ClippedPercent float64
	MeanAmplitude  float64
}

// GetIQStats returns accumulated IQ sample statistics from libdump978.
// If reset is true, the underlying counters are zeroed after reading.
func GetIQStats(reset bool) IQStats {
	processMu.Lock()
	defer processMu.Unlock()

	var total, clipped, absSum C.uint64_t
	var cReset C.int
	if reset {
		cReset = 1
	}
	C.dump978_get_iq_stats(&total, &clipped, &absSum, cReset)

	stats := IQStats{
		TotalSamples:   uint64(total),
		ClippedSamples: uint64(clipped),
	}
	if stats.TotalSamples > 0 {
		stats.ClippedPercent = (float64(stats.ClippedSamples) * 100.0) / float64(stats.TotalSamples)
		stats.MeanAmplitude = float64(absSum) / float64(stats.TotalSamples)
	}
	return stats
}

// GetDroppedFrames returns the total number of demodulated frames dropped because OutChan was full.
func GetDroppedFrames() uint64 {
	return DroppedFrames.Load()
}

func ProcessDataFromChannel() {
	for inData := range InChan {
		ProcessData(inData)
	}
}
