package godump978

import (
	"math"
	"sync"
	"sync/atomic"
	"testing"
)

func TestProcessDataEdgeCasesAndConcurrency(t *testing.T) {
	// Empty and nil slices must not panic.
	ProcessData(nil)
	ProcessData([]byte{})

	// Small buffers (< 17952 bytes) previously triggered negative return from
	// process_buffer() and out-of-bounds memmove under-reads in C.
	for _, sz := range []int{1, 2, 3, 16, 100, 4096, 17950} {
		buf := make([]byte, sz)
		ProcessData(buf)
	}

	// Concurrent ProcessData calls must be race-free.
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 2048)
			for i := 0; i < 10; i++ {
				ProcessData(buf)
			}
		}()
	}
	wg.Wait()
}

func TestProcessDataFromChannelClose(t *testing.T) {
	orig := InChan
	ch := make(chan []byte, 4)
	InChan = ch
	defer func() { InChan = orig }()

	done := make(chan struct{})
	go func() {
		defer close(done)
		ProcessDataFromChannel()
	}()

	ch <- []byte{0x80, 0x80, 0x80, 0x80}
	ch <- nil
	close(ch)

	<-done
}

func assertAlmostEqual(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Fatalf("%s = %.6f, want %.6f (tol %.6f)", name, got, want, tol)
	}
}

func TestGetIQStats(t *testing.T) {
	// Drain any counts left over from earlier tests.
	_ = GetIQStats(true)

	empty := GetIQStats(false)
	if empty.TotalSamples != 0 || empty.ClippedSamples != 0 || empty.ClippedPercent != 0 || empty.MeanAmplitude != 0 {
		t.Fatalf("expected zeroed stats after reset, got %+v", empty)
	}

	// Quiet buffer (0x80 = DC center 128): 0% clipped, 0 mean amplitude.
	quiet := make([]byte, 1024)
	for i := range quiet {
		quiet[i] = 0x80
	}
	ProcessData(quiet)

	peek := GetIQStats(false)
	if peek.TotalSamples != 1024 || peek.ClippedSamples != 0 {
		t.Fatalf("quiet peek: got %+v", peek)
	}
	assertAlmostEqual(t, "quiet ClippedPercent", peek.ClippedPercent, 0.0, 1e-9)
	assertAlmostEqual(t, "quiet MeanAmplitude", peek.MeanAmplitude, 0.0, 1e-9)

	// reset=false must preserve counters; reset=true must return them and clear.
	got := GetIQStats(true)
	if got != peek {
		t.Fatalf("quiet reset read %+v != peek %+v", got, peek)
	}
	if after := GetIQStats(false); after.TotalSamples != 0 || after.ClippedSamples != 0 || after.ClippedPercent != 0 || after.MeanAmplitude != 0 {
		t.Fatalf("expected zeroed stats after quiet reset, got %+v", after)
	}

	// Saturated low buffer (0x00): 100% clipped, mean amplitude = 128.
	satLow := make([]byte, 512)
	ProcessData(satLow)
	got = GetIQStats(true)
	if got.TotalSamples != 512 || got.ClippedSamples != 512 {
		t.Fatalf("satLow: got %+v", got)
	}
	assertAlmostEqual(t, "satLow ClippedPercent", got.ClippedPercent, 100.0, 1e-9)
	assertAlmostEqual(t, "satLow MeanAmplitude", got.MeanAmplitude, 128.0, 1e-9)

	// Saturated high buffer (0xFF): 100% clipped, mean amplitude = 127.
	satHigh := make([]byte, 512)
	for i := range satHigh {
		satHigh[i] = 0xFF
	}
	ProcessData(satHigh)
	got = GetIQStats(true)
	if got.TotalSamples != 512 || got.ClippedSamples != 512 {
		t.Fatalf("satHigh: got %+v", got)
	}
	assertAlmostEqual(t, "satHigh ClippedPercent", got.ClippedPercent, 100.0, 1e-9)
	assertAlmostEqual(t, "satHigh MeanAmplitude", got.MeanAmplitude, 127.0, 1e-9)

	// Mixed buffer:
	//   50x 0x00 (clipped, |0-128|=128)
	//   50x 0xFF (clipped, |255-128|=127)
	//  100x 0x80 (unclipped, 0)
	//  100x 0x90 (unclipped, 16)
	//  100x 0x70 (unclipped, 16)
	// Total = 400, Clipped = 100 (25%), absSum = 6400+6350+0+1600+1600 = 15950 -> Mean = 39.875.
	mixed := make([]byte, 0, 400)
	for i := 0; i < 50; i++ {
		mixed = append(mixed, 0x00)
	}
	for i := 0; i < 50; i++ {
		mixed = append(mixed, 0xFF)
	}
	for i := 0; i < 100; i++ {
		mixed = append(mixed, 0x80)
	}
	for i := 0; i < 100; i++ {
		mixed = append(mixed, 0x90)
	}
	for i := 0; i < 100; i++ {
		mixed = append(mixed, 0x70)
	}
	ProcessData(mixed)

	got = GetIQStats(true)
	if got.TotalSamples != 400 || got.ClippedSamples != 100 {
		t.Fatalf("mixed: got %+v", got)
	}
	assertAlmostEqual(t, "mixed ClippedPercent", got.ClippedPercent, 25.0, 1e-9)
	assertAlmostEqual(t, "mixed MeanAmplitude", got.MeanAmplitude, 39.875, 1e-9)

	// Boundary non-clipped values: 0x01 (amp 127) and 0xFE (amp 126).
	ProcessData([]byte{0x01, 0xFE})
	got = GetIQStats(true)
	if got.TotalSamples != 2 || got.ClippedSamples != 0 {
		t.Fatalf("boundary non-clipped: got %+v", got)
	}
	assertAlmostEqual(t, "boundary ClippedPercent", got.ClippedPercent, 0.0, 1e-9)
	assertAlmostEqual(t, "boundary MeanAmplitude", got.MeanAmplitude, 126.5, 1e-9)
}

func TestGetIQStatsConcurrency(t *testing.T) {
	_ = GetIQStats(true)

	const (
		writers    = 4
		iters      = 25
		bufSize    = 1024
		clippedPer = 256
	)

	buf := make([]byte, bufSize)
	for i := 0; i < bufSize; i++ {
		switch {
		case i < clippedPer/2:
			buf[i] = 0x00
		case i < clippedPer:
			buf[i] = 0xFF
		default:
			buf[i] = 0x80
		}
	}

	var totalSeen atomic.Uint64
	var clippedSeen atomic.Uint64
	stopReaders := make(chan struct{})

	var readerWG sync.WaitGroup
	for r := 0; r < 2; r++ {
		reset := r == 0
		readerWG.Add(1)
		go func(doReset bool) {
			defer readerWG.Done()
			for {
				select {
				case <-stopReaders:
					return
				default:
					s := GetIQStats(doReset)
					if doReset {
						totalSeen.Add(s.TotalSamples)
						clippedSeen.Add(s.ClippedSamples)
					}
				}
			}
		}(reset)
	}

	var writerWG sync.WaitGroup
	for w := 0; w < writers; w++ {
		writerWG.Add(1)
		go func() {
			defer writerWG.Done()
			for i := 0; i < iters; i++ {
				ProcessData(buf)
			}
		}()
	}

	writerWG.Wait()
	close(stopReaders)
	readerWG.Wait()

	rem := GetIQStats(true)
	gotTotal := totalSeen.Load() + rem.TotalSamples
	gotClipped := clippedSeen.Load() + rem.ClippedSamples

	wantTotal := uint64(writers * iters * bufSize)
	wantClipped := uint64(writers * iters * clippedPer)
	if gotTotal != wantTotal || gotClipped != wantClipped {
		t.Fatalf("concurrent stats mismatch: got total=%d clipped=%d, want total=%d clipped=%d",
			gotTotal, gotClipped, wantTotal, wantClipped)
	}
}

func TestGetDroppedFrames(t *testing.T) {
	orig := DroppedFrames.Load()
	defer DroppedFrames.Store(orig)

	DroppedFrames.Store(42)
	if got := GetDroppedFrames(); got != 42 {
		t.Fatalf("GetDroppedFrames() = %d, want 42", got)
	}
	DroppedFrames.Add(5)
	if got := GetDroppedFrames(); got != 47 {
		t.Fatalf("GetDroppedFrames() = %d, want 47", got)
	}
}
