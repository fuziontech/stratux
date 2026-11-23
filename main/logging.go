/*
	Copyright (c) 2023 Adrian Batzill
	Distributable under the terms of The "BSD New" License
	that can be found in the LICENSE file, herein included
	as part of this header.

	logging.go: Initialize go logging, watch log file size and rotate, delete old logs

*/

package main

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ricochet2200/go-disk-usage/du"
)

const debugLogFile = "stratux.log"

var debugLogf string // Set according to OS config.
var logFileHandle *os.File
var logFileMutex sync.Mutex

func effectiveLogDir() string {
	if logDirf != "" {
		return logDirf
	}
	return logDir
}

func parseLogSuffix(path string) (int, bool) {
	base := filepath.Base(path)
	prefix := debugLogFile + "."
	if !strings.HasPrefix(base, prefix) {
		return 0, false
	}
	n, err := strconv.Atoi(base[len(prefix):])
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

func getStratuxLogFiles() []string {
	dir := effectiveLogDir()
	entries, err := os.ReadDir(dir)
	stratuxLogs := make([]string, 0)
	if err != nil {
		return stratuxLogs
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		fullPath := filepath.Join(dir, e.Name())
		if _, ok := parseLogSuffix(fullPath); ok {
			stratuxLogs = append(stratuxLogs, fullPath)
		}
	}
	sort.Slice(stratuxLogs, func(i, j int) bool {
		ni, _ := parseLogSuffix(stratuxLogs[i])
		nj, _ := parseLogSuffix(stratuxLogs[j])
		return ni < nj
	})
	return stratuxLogs
}

func rotateLogs() {
	logFileMutex.Lock()
	defer logFileMutex.Unlock()

	dir := effectiveLogDir()
	stratuxLogs := getStratuxLogFiles()

	// Rename suffix, remove if >= 9
	for i := len(stratuxLogs) - 1; i >= 0; i-- {
		logNum, ok := parseLogSuffix(stratuxLogs[i])
		if !ok {
			continue
		}

		if logNum >= 9 {
			if err := os.Remove(stratuxLogs[i]); err != nil {
				log.Printf("Failed to remove rotated log '%s': %s\n", stratuxLogs[i], err.Error())
			}
		} else {
			newPath := filepath.Join(dir, debugLogFile+"."+strconv.Itoa(logNum+1))
			if err := os.Rename(stratuxLogs[i], newPath); err != nil {
				log.Printf("Failed to rename rotated log '%s' to '%s': %s\n", stratuxLogs[i], newPath, err.Error())
			}
		}
	}

	// Now rename current log file and re-open
	if err := os.Rename(debugLogf, debugLogf+".1"); err != nil && !os.IsNotExist(err) {
		log.Printf("Failed to rotate current log '%s': %s\n", debugLogf, err.Error())
	}
	openLogFileLocked()
}

func deleteOldestLog() (int64, bool) {
	logs := getStratuxLogFiles()
	if len(logs) == 0 {
		return 0, false
	}
	oldest := logs[len(logs)-1]
	var size int64
	if stat, err := os.Stat(oldest); err == nil {
		size = stat.Size()
	}
	if err := os.Remove(oldest); err != nil {
		log.Printf("Failed to remove old log '%s': %s\n", oldest, err.Error())
		return 0, false
	}
	return size, true
}

func logFileSize() int64 {
	logFileMutex.Lock()
	defer logFileMutex.Unlock()
	if logFileHandle == nil {
		return 0
	}
	fileInfo, err := logFileHandle.Stat()
	if err != nil {
		return 0
	}
	return fileInfo.Size()
}

func clearDebugLogFile() {
	logFileMutex.Lock()
	if logFileHandle == nil {
		logFileMutex.Unlock()
		return
	}
	_, err := logFileHandle.Seek(0, 0)
	if err != nil {
		logFileMutex.Unlock()
		log.Printf("Could not seek to the beginning of the logfile\n")
		return
	}
	err2 := logFileHandle.Truncate(0)
	logFileMutex.Unlock()
	if err2 != nil {
		log.Printf("Could not truncate the logfile\n")
		return
	}
	log.Printf("Logfile truncated\n")
}

func logFileWatcher() {
	for {
		logSize, err := os.Stat(debugLogf)
		if err == nil && logSize.Size() > 10*1024*1024 { // 10mb limit
			rotateLogs()
		}

		usage := du.NewDiskUsage(effectiveLogDir())
		freeBytes := int64(usage.Free())
		for freeBytes < 50*1024*1024 { // leave 50mb free
			deleted, ok := deleteOldestLog()
			if !ok {
				break
			}
			freeBytes += deleted
		}

		time.Sleep(30 * time.Second)
	}
}

func openLogFileLocked() {
	oldFp := logFileHandle
	debugLogf = filepath.Join(effectiveLogDir(), debugLogFile)
	fp, err := os.OpenFile(debugLogf, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		addSingleSystemErrorf(debugLogf, "Failed to open '%s': %s", debugLogf, err.Error())
		return
	}

	// Keep the logfile handle for later use
	logFileHandle = fp
	mfp := io.MultiWriter(fp, os.Stdout)
	log.SetOutput(mfp)

	// Make sure crash dumps are written to the log as well
	_ = syscall.Dup3(int(fp.Fd()), 2, 0)

	if oldFp != nil {
		_ = oldFp.Close()
	}
}

func openLogFile() {
	logFileMutex.Lock()
	defer logFileMutex.Unlock()
	openLogFileLocked()
}

func initLogging() {
	openLogFile()
	go logFileWatcher()
}

// Log always
func logInf(msg string, args ...any) { log.Printf(msg, args...) }

// Log on error (no special processing right now)
func logErr(msg string, args ...any) { log.Printf(msg, args...) }

// Log if debug enabled ("verbose message log" in settings tab)
func logDbg(msg string, args ...any) {
	globalSettingsMutex.RLock()
	dbg := globalSettings.DEBUG
	globalSettingsMutex.RUnlock()
	if dbg {
		log.Printf(msg, args...)
	}
}
