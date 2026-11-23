// Copyright (c) 2023 Adrian Batzill
// Distributable under the terms of The "BSD New" License
// that can be found in the LICENSE file, herein included
// as part of this header.
// trace.go: record all incoming messages (GPS, dump1090, ...) for future replay

package main

import (
	"compress/gzip"
	"encoding/csv"
	"log"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ricochet2200/go-disk-usage/du"
)

type traceRecord struct {
	ts      string
	context string
	data    []byte
}

type traceSession struct {
	records chan traceRecord
	stop    chan struct{}
	flush   chan chan struct{}
	done    chan struct{}
}

type TraceLogger struct {
	fileHandle        *os.File
	gzWriter          *gzip.Writer
	csvWriter         *csv.Writer
	fileName          string
	hasProperFilename bool
	traceMutex        sync.Mutex
	active            atomic.Bool
	replaying         atomic.Bool
	session           atomic.Pointer[traceSession]
}

const (
	CONTEXT_AIS         = "ais"
	CONTEXT_NMEA        = "nmea"
	CONTEXT_APRS        = "aprs"
	CONTEXT_OGN_RX      = "ogn-rx"
	CONTEXT_DUMP1090    = "dump1090"
	CONTEXT_GODUMP978   = "godump978"
	CONTEXT_LOWPOWERUAT = "lowpower_uat"
)

var TraceLog TraceLogger

func traceDir() string {
	base := logDirf
	if base == "" {
		base = logDir
	}
	return filepath.Join(base, "stratux")
}

// At startup, we usually don't know the precise time to generate a good filename.
// Therefore, once we receive our first valid timestamp, we will rename the file to something more appropriate
func (tracer *TraceLogger) OnTimestamp(ts time.Time) {
	tracer.traceMutex.Lock()
	defer tracer.traceMutex.Unlock()
	if tracer.fileHandle == nil || tracer.hasProperFilename {
		return
	}
	formatted := ts.Format(time.RFC3339)
	fname := filepath.Join(traceDir(), formatted+"_trace.txt.gz")
	if tracer.fileName != fname {
		if err := os.Rename(tracer.fileName, fname); err != nil {
			log.Printf("Failed to rename trace log file to %s: %s", fname, err.Error())
			return
		}
		tracer.fileName = fname
	}
	tracer.hasProperFilename = true
}

// Context for now may be one of CONTEXT_*
func (tracer *TraceLogger) Record(context string, data []byte) {
	if !tracer.active.Load() || tracer.replaying.Load() {
		return
	}
	sess := tracer.session.Load()
	if sess == nil {
		return
	}
	rec := traceRecord{
		ts:      stratuxClock.GetTime().Format(time.RFC3339Nano),
		context: context,
		data:    append([]byte(nil), data...),
	}
	select {
	case sess.records <- rec:
	default:
		// Drop if bounded channel is full rather than blocking RF/GPS pipelines.
	}
}

func (tracer *TraceLogger) writerLoop(sess *traceSession, fh *os.File, gzw *gzip.Writer, csvw *csv.Writer) {
	defer close(sess.done)

	drain := func() {
		for {
			select {
			case rec := <-sess.records:
				_ = csvw.Write([]string{rec.ts, rec.context, string(rec.data)})
			default:
				return
			}
		}
	}

	for {
		select {
		case rec := <-sess.records:
			_ = csvw.Write([]string{rec.ts, rec.context, string(rec.data)})
		case ack := <-sess.flush:
			drain()
			csvw.Flush()
			_ = gzw.Flush()
			_ = fh.Sync()
			close(ack)
		case <-sess.stop:
			drain()
			csvw.Flush()
			_ = gzw.Close()
			_ = fh.Close()
			return
		}
	}
}

func (tracer *TraceLogger) Flush() {
	if !tracer.active.Load() {
		return
	}
	sess := tracer.session.Load()
	if sess == nil {
		return
	}
	ack := make(chan struct{})
	select {
	case sess.flush <- ack:
		select {
		case <-ack:
		case <-sess.done:
		}
	case <-sess.done:
	}
}

func (tracer *TraceLogger) Start() {
	tracer.traceMutex.Lock()
	defer tracer.traceMutex.Unlock()
	if tracer.fileHandle != nil {
		return
	}
	dir := traceDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Printf("Failed to create trace log directory %s: %s", dir, err.Error())
		return
	}
	ts := time.Now().UTC().Format(time.RFC3339)
	fname := filepath.Join(dir, ts+"_trace.txt.gz")

	fileHandle, err := os.OpenFile(fname, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0666)
	if err != nil {
		log.Printf("Failed to open trace log file: %s", err.Error())
		return
	}
	gzWriter := gzip.NewWriter(fileHandle)
	csvWriter := csv.NewWriter(gzWriter)
	sess := &traceSession{
		records: make(chan traceRecord, 4096),
		stop:    make(chan struct{}),
		flush:   make(chan chan struct{}, 1),
		done:    make(chan struct{}),
	}

	tracer.gzWriter = gzWriter
	tracer.csvWriter = csvWriter
	tracer.fileHandle = fileHandle
	tracer.fileName = fname
	tracer.hasProperFilename = false
	tracer.session.Store(sess)
	tracer.active.Store(true)
	go tracer.writerLoop(sess, fileHandle, gzWriter, csvWriter)
}

func (tracer *TraceLogger) Stop() {
	tracer.traceMutex.Lock()
	defer tracer.traceMutex.Unlock()
	if tracer.fileHandle == nil {
		return
	}
	tracer.active.Store(false)
	sess := tracer.session.Swap(nil)
	tracer.fileHandle = nil
	tracer.csvWriter = nil
	tracer.gzWriter = nil
	if sess != nil {
		close(sess.stop)
		<-sess.done
	}
}

func (tracer *TraceLogger) IsActive() bool {
	return tracer.active.Load()
}

func (tracer *TraceLogger) IsReplaying() bool {
	return tracer.replaying.Load()
}

func (tracer *TraceLogger) Replay(fname string, speedMultiplier float64, traceSkip int64, msgtypes []string) {
	if speedMultiplier <= 0 || math.IsNaN(speedMultiplier) || math.IsInf(speedMultiplier, 0) {
		log.Printf("Invalid trace replay speed multiplier %v, defaulting to 1.0", speedMultiplier)
		speedMultiplier = 1.0
	}
	fhandle, err := os.Open(fname)
	if err != nil {
		log.Printf("Failed to open trace file %s: %s", fname, err.Error())
		return
	}
	defer fhandle.Close()

	gzReader, err := gzip.NewReader(fhandle)
	if err != nil {
		log.Printf("Failed to open gzip stream for file %s: %s", fname, err.Error())
		return
	}
	defer gzReader.Close()

	tracer.replaying.Store(true)
	defer tracer.replaying.Store(false)

	startTs := time.Time{}.Add(time.Duration(traceSkip) * time.Minute)
	csvReader := csv.NewReader(gzReader)
	for {
		fields, err := csvReader.Read()
		if err != nil {
			log.Printf("Trace replay stopped: %s", err)
			break
		}
		if len(fields) != 3 {
			log.Printf("Failed to parse trace line")
			continue
		}
		context := fields[1]
		if len(msgtypes) > 0 && !slices.Contains(msgtypes, context) {
			continue
		}
		ts, err := time.Parse(time.RFC3339Nano, fields[0])
		if err != nil {
			log.Printf("Failed to parse trace log timestamp %s", fields[0])
			continue
		}
		if ts.Before(startTs) {
			continue
		}
		ts = ts.Add(-time.Duration(traceSkip) * time.Minute)

		millis := float64(ts.Sub(time.Time{}).Milliseconds()) / speedMultiplier
		ts = time.Time{}.Add(time.Duration(millis) * time.Millisecond)
		injectTraceMessage(context, ts, []byte(fields[2]))
	}
}

func injectTraceMessage(context string, ts time.Time, data []byte) {
	toWait := ts.Sub(stratuxClock.GetTime())
	if toWait > 0 {
		time.Sleep(toWait)
	}

	if context == CONTEXT_AIS {
		parseAisMessage(string(data))
	} else if context == CONTEXT_NMEA {
		globalStatusMutex.Lock()
		globalStatus.GPS_connected = true
		globalStatusMutex.Unlock()
		processNMEALineLow(string(data), true)
	} else if context == CONTEXT_APRS {
		parseAprsMessage(string(data), true)
	} else if context == CONTEXT_OGN_RX {
		parseOgnMessage(string(data), true)
	} else if context == CONTEXT_DUMP1090 {
		parseDump1090Message(string(data))
	} else if context == CONTEXT_GODUMP978 {
		handleUatMessage(string(data))
	} else if context == CONTEXT_LOWPOWERUAT {
		processRadioMessage(data)
	}
}

func traceLoggerWatchdog() {
	for {
		if TraceLog.IsReplaying() {
			time.Sleep(1 * time.Second)
			continue
		}

		if TraceLog.IsActive() {
			usage := du.NewDiskUsage("/")
			if usage.Free() < 1024*1024*50 {
				// less than 50mb free? deactivate
				log.Printf("Space running out - disable trace logging for this run")
				TraceLog.Stop()
				break
			}
		}

		globalSettingsMutex.RLock()
		wantTrace := globalSettings.TraceLog
		globalSettingsMutex.RUnlock()

		if TraceLog.IsActive() && !wantTrace {
			TraceLog.Stop()
		} else if !TraceLog.IsActive() && wantTrace {
			TraceLog.Start()
		}
		time.Sleep(1 * time.Second)
		TraceLog.Flush()
	}
}
