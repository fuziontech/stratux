/*
	Copyright (c) 2015-2016 Christopher Young
	Distributable under the terms of The "BSD New" License
	that can be found in the LICENSE file, herein included
	as part of this header.

	datalog.go: Log stratux data as it is received. Bucket data into timestamp time slots.

*/

package main

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const (
	LOG_TIMESTAMP_RESOLUTION = 250 * time.Millisecond
	maxTimestampWindow       = 1000 // Keep recent timestamp buckets (~250s at 4 Hz) to bound memory
)

type StratuxTimestamp struct {
	id                   int64
	Time_type_preference int // 0 = stratuxClock, 1 = gpsClock, 2 = gpsClock extrapolated via stratuxClock.
	StratuxClock_value   time.Time
	GPSClock_value       time.Time // The value of this is either from the GPS or extrapolated from the GPS via stratuxClock if pref is 1 or 2. It is time.Time{} if 0.
	PreferredTime_value  time.Time
	StartupID            int64
}

// 'startup' table creates a new entry each time the daemon is started. This keeps track of sequential starts, even if the
//
//	timestamp is ambiguous (units with no GPS). This struct is just a placeholder for an empty table (other than primary key).
type StratuxStartup struct {
	id   int64
	Fill string
}

var dataLogStarted bool
var dataLogReady atomic.Bool

var (
	dataLogMu     sync.Mutex
	dataLogStopCh chan struct{}
	dataLogDoneCh chan struct{}
)

var stratuxStartupID int64

var (
	dataLogTimestampsMutex sync.Mutex
	dataLogTimestamps      map[int64]StratuxTimestamp
	dataLogCurTimestamp    int64 // Current timestamp bucket key in dataLogTimestamps (not necessarily the db id).
)

// sqlExecer is implemented by both *sql.DB and *sql.Tx so batch inserts run inside transactions.
type sqlExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func quoteSQLIdent(ident string) string {
	return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
}

func pruneOldTimestampsLocked() {
	if len(dataLogTimestamps) <= maxTimestampWindow {
		return
	}
	cutoff := dataLogCurTimestamp - maxTimestampWindow
	for k := range dataLogTimestamps {
		if k < cutoff {
			delete(dataLogTimestamps, k)
		}
	}
}

/*
	checkTimestamp().
		Verify that our current timestamp is within the LOG_TIMESTAMP_RESOLUTION bucket.
		 Returns the current bucket key (creating a new bucket if the current one expired).
		 This is where GPS timestamps are extrapolated, if the GPS data is currently valid.
*/

func checkTimestamp() int64 {
	now := stratuxClock.GetTime()
	gpsClockValid := isGPSClockValid()

	dataLogTimestampsMutex.Lock()
	defer dataLogTimestampsMutex.Unlock()

	if dataLogTimestamps == nil {
		dataLogTimestamps = make(map[int64]StratuxTimestamp)
	}

	curTS, ok := dataLogTimestamps[dataLogCurTimestamp]
	if !ok || now.Sub(curTS.StratuxClock_value) >= LOG_TIMESTAMP_RESOLUTION {
		var ts StratuxTimestamp
		ts.id = 0
		ts.Time_type_preference = 0 // stratuxClock.
		ts.StratuxClock_value = now
		ts.GPSClock_value = time.Time{}
		ts.PreferredTime_value = now

		// Extrapolate from GPS timestamp, if possible.
		if gpsClockValid && ok && (curTS.Time_type_preference == 1 || curTS.Time_type_preference == 2) {
			timeSinceLastTS := ts.StratuxClock_value.Sub(curTS.StratuxClock_value)
			extrapolatedGPSTimestamp := curTS.PreferredTime_value.Add(timeSinceLastTS)

			ts.Time_type_preference = 2
			ts.PreferredTime_value = extrapolatedGPSTimestamp
			ts.GPSClock_value = extrapolatedGPSTimestamp
		}

		dataLogCurTimestamp++
		dataLogTimestamps[dataLogCurTimestamp] = ts
		pruneOldTimestampsLocked()
	}
	return dataLogCurTimestamp
}

type SQLiteMarshal struct {
	FieldType string
	Marshal   func(v reflect.Value) string
}

func boolMarshal(v reflect.Value) string {
	b := v.Bool()
	if b {
		return "1"
	}
	return "0"
}

func intMarshal(v reflect.Value) string {
	return strconv.FormatInt(v.Int(), 10)
}

func uintMarshal(v reflect.Value) string {
	return strconv.FormatUint(v.Uint(), 10)
}

func floatMarshal(v reflect.Value) string {
	return strconv.FormatFloat(v.Float(), 'f', 10, 64)
}

func stringMarshal(v reflect.Value) string {
	return v.String()
}

func notsupportedMarshal(v reflect.Value) string {
	return ""
}

func structCanBeMarshalled(v reflect.Value) bool {
	m := v.MethodByName("String")
	if m.IsValid() && !m.IsNil() {
		return true
	}
	return false
}

func structMarshal(v reflect.Value) string {
	if structCanBeMarshalled(v) {
		m := v.MethodByName("String")
		in := make([]reflect.Value, 0)
		ret := m.Call(in)
		if len(ret) > 0 {
			return ret[0].String()
		}
	}
	return ""
}

var sqliteMarshalFunctions = map[string]SQLiteMarshal{
	"bool":         {FieldType: "INTEGER", Marshal: boolMarshal},
	"int":          {FieldType: "INTEGER", Marshal: intMarshal},
	"uint":         {FieldType: "INTEGER", Marshal: uintMarshal},
	"float":        {FieldType: "REAL", Marshal: floatMarshal},
	"string":       {FieldType: "TEXT", Marshal: stringMarshal},
	"struct":       {FieldType: "STRING", Marshal: structMarshal},
	"notsupported": {FieldType: "notsupported", Marshal: notsupportedMarshal},
}

var sqlTypeMap = map[reflect.Kind]string{
	reflect.Bool:          "bool",
	reflect.Int:           "int",
	reflect.Int8:          "int",
	reflect.Int16:         "int",
	reflect.Int32:         "int",
	reflect.Int64:         "int",
	reflect.Uint:          "uint",
	reflect.Uint8:         "uint",
	reflect.Uint16:        "uint",
	reflect.Uint32:        "uint",
	reflect.Uint64:        "uint",
	reflect.Uintptr:       "notsupported",
	reflect.Float32:       "float",
	reflect.Float64:       "float",
	reflect.Complex64:     "notsupported",
	reflect.Complex128:    "notsupported",
	reflect.Array:         "notsupported",
	reflect.Chan:          "notsupported",
	reflect.Func:          "notsupported",
	reflect.Interface:     "notsupported",
	reflect.Map:           "notsupported",
	reflect.Ptr:           "notsupported",
	reflect.Slice:         "notsupported",
	reflect.String:        "string",
	reflect.Struct:        "struct",
	reflect.UnsafePointer: "notsupported",
}

// shouldLogField unifies struct-field filtering between makeTable and insertData
// so schemas and INSERT statements always match.
func shouldLogField(val reflect.Value, idx int) (fieldName string, sqlTypeAlias string, ok bool) {
	field := val.Field(idx)
	kind := field.Kind()
	fieldName = val.Type().Field(idx).Name
	sqlTypeAlias = sqlTypeMap[kind]

	if sqlTypeAlias == "" || sqlTypeAlias == "notsupported" || fieldName == "id" {
		return "", "", false
	}
	if sqlTypeAlias == "struct" && !structCanBeMarshalled(field) {
		return "", "", false
	}
	return fieldName, sqlTypeAlias, true
}

func makeTable(i any, tbl string, db *sql.DB) {
	val := reflect.ValueOf(i)

	fields := make([]string, 0)
	for idx := 0; idx < val.NumField(); idx++ {
		fieldName, sqlTypeAlias, ok := shouldLogField(val, idx)
		if !ok {
			continue
		}
		sqlType := sqliteMarshalFunctions[sqlTypeAlias].FieldType
		s := quoteSQLIdent(fieldName) + " " + sqlType
		fields = append(fields, s)
	}

	// Add the timestamp_id field to link up with the timestamp table.
	if tbl != "timestamp" && tbl != "startup" {
		fields = append(fields, quoteSQLIdent("timestamp_id")+" INTEGER")
	}

	tblCreate := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (id INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT, %s)", quoteSQLIdent(tbl), strings.Join(fields, ", "))

	_, err := db.Exec(tblCreate)
	if err != nil {
		log.Printf("datalog.go: makeTable(%s) ERROR: %s\n", tbl, err.Error())
	}
}

// dataLogBatch holds per-writer cached INSERT statements and queued row values.
type dataLogBatch struct {
	insertString   map[string]string // INSERT INTO tbl (col1, col2, ...) VALUES(?, ?, ...). Only for one value.
	insertBatchIfs map[string][][]any
}

func newDataLogBatch() *dataLogBatch {
	return &dataLogBatch{
		insertString:   make(map[string]string),
		insertBatchIfs: make(map[string][][]any),
	}
}

/*
bulkInsert().

	Reads insertString and insertBatchIfs. This is called after a group of insertData() calls.
*/
func (b *dataLogBatch) bulkInsert(tbl string, exec sqlExecer) (res sql.Result, err error) {
	// Always clear buffers for this table on return (even on error) so failed batches never leak RAM.
	defer func() {
		delete(b.insertString, tbl)
		delete(b.insertBatchIfs, tbl)
	}()

	baseStmt, ok := b.insertString[tbl]
	if !ok {
		return nil, errors.New("no insert statement")
	}

	batchVals := b.insertBatchIfs[tbl]
	if len(batchVals) == 0 {
		return nil, nil
	}

	numColsPerRow := len(batchVals[0])
	if numColsPerRow == 0 {
		return nil, errors.New("zero columns in batch")
	}
	maxRowBatch := 999 / numColsPerRow // SQLITE_MAX_VARIABLE_NUMBER = 999.
	if maxRowBatch < 1 {
		maxRowBatch = 1
	}

	rowPlaceholder := ", (" + strings.Join(strings.Split(strings.Repeat("?", numColsPerRow), ""), ",") + ")"

	for len(batchVals) > 0 {
		i := 0
		var stmtBuilder strings.Builder
		vals := make([]any, 0)
		querySize := uint64(0)                                            // Size of the query in bytes.
		for len(batchVals) > 0 && i < maxRowBatch && querySize < 750000 { // Maximum of 1,000,000 bytes per query.
			if stmtBuilder.Len() == 0 { // The first set will be covered by insertString.
				stmtBuilder.WriteString(baseStmt)
				querySize += uint64(len(baseStmt))
			} else {
				stmtBuilder.WriteString(rowPlaceholder)
				querySize += uint64(len(rowPlaceholder))
			}
			for _, val := range batchVals[0] {
				if s, ok := val.(string); ok {
					querySize += uint64(len(s))
				}
			}
			vals = append(vals, batchVals[0]...)
			batchVals = batchVals[1:]
			i++
		}
		res, err = exec.Exec(stmtBuilder.String(), vals...)
		if err != nil {
			log.Printf("sqlite INSERT error (%s): '%s'\n", tbl, err.Error())
			return res, err
		}
	}

	return res, nil
}

/*
insertData().

	Inserts an arbitrary struct into an SQLite table.
	 Inserts the timestamp first, if its 'id' is 0.
*/
func (b *dataLogBatch) insertData(i any, tbl string, exec sqlExecer, ts_num int64) int64 {
	val := reflect.ValueOf(i)

	keys := make([]string, 0)
	values := make([]string, 0)
	for idx := 0; idx < val.NumField(); idx++ {
		fieldName, sqlTypeAlias, ok := shouldLogField(val, idx)
		if !ok {
			continue
		}

		v := sqliteMarshalFunctions[sqlTypeAlias].Marshal(val.Field(idx))
		keys = append(keys, quoteSQLIdent(fieldName))
		values = append(values, v)
	}

	// Add the timestamp_id field to link up with the timestamp table.
	if tbl != "timestamp" && tbl != "startup" {
		keys = append(keys, quoteSQLIdent("timestamp_id"))

		dataLogTimestampsMutex.Lock()
		ts, ok := dataLogTimestamps[ts_num]
		if !ok {
			now := stratuxClock.GetTime()
			ts = StratuxTimestamp{
				id:                   0,
				Time_type_preference: 0,
				StratuxClock_value:   now,
				PreferredTime_value:  now,
			}
		}
		tsID := ts.id
		dataLogTimestampsMutex.Unlock()

		if tsID == 0 {
			ts.StartupID = stratuxStartupID
			tsID = b.insertData(ts, "timestamp", exec, ts_num)
			if tsID != 0 {
				dataLogTimestampsMutex.Lock()
				if cur, exists := dataLogTimestamps[ts_num]; exists {
					cur.id = tsID
					cur.StartupID = stratuxStartupID
					dataLogTimestamps[ts_num] = cur
				}
				dataLogTimestampsMutex.Unlock()
			}
		}
		values = append(values, strconv.FormatInt(tsID, 10))
	}

	if _, ok := b.insertString[tbl]; !ok {
		tblInsert := fmt.Sprintf("INSERT INTO %s (%s) VALUES(%s)", quoteSQLIdent(tbl), strings.Join(keys, ","),
			strings.Join(strings.Split(strings.Repeat("?", len(keys)), ""), ","))
		b.insertString[tbl] = tblInsert
	}

	ifs := make([]any, len(values))
	for idx := 0; idx < len(values); idx++ {
		ifs[idx] = values[idx]
	}

	b.insertBatchIfs[tbl] = append(b.insertBatchIfs[tbl], ifs)

	if tbl == "timestamp" || tbl == "startup" { // Immediate insert always for "timestamp" and "startup" table.
		res, err := b.bulkInsert(tbl, exec) // Bulk insert of 1, always.
		if err == nil && res != nil {
			id, err := res.LastInsertId()
			if err == nil {
				return id
			}
		}
	}

	return 0
}

type DataLogRow struct {
	tbl    string
	data   any
	ts_num int64
}

// Allocated synchronously at init and never closed so non-blocking log*() calls can never panic.
var dataLogChan = make(chan DataLogRow, 10240)

func dataLogWriter(db *sql.DB, writeChan <-chan DataLogRow, writerDone chan struct{}) {
	defer close(writerDone)

	writeTicker := time.NewTicker(10 * time.Second)
	defer writeTicker.Stop()

	batch := newDataLogBatch()
	rowsQueuedForWrite := make([]DataLogRow, 0)

	flushRows := func(rows []DataLogRow) {
		if len(rows) == 0 {
			return
		}
		timeStart := stratuxClock.GetTime()
		nRows := len(rows)

		globalSettingsMutex.RLock()
		debug := globalSettings.DEBUG
		globalSettingsMutex.RUnlock()

		if debug {
			log.Printf("Writing %d rows\n", nRows)
		}

		// Start transaction.
		tx, err := db.Begin()
		if err != nil {
			log.Printf("db.Begin() error: %s\n", err.Error())
			return
		}

		tblsAffected := make(map[string]bool)
		for _, r := range rows {
			tblsAffected[r.tbl] = true
			batch.insertData(r.data, r.tbl, tx, r.ts_num)
		}

		// Execute all bulk inserts inside tx.
		var flushErr error
		for tbl := range tblsAffected {
			if _, err := batch.bulkInsert(tbl, tx); err != nil {
				flushErr = err
			}
		}

		if flushErr != nil {
			if rbErr := tx.Rollback(); rbErr != nil {
				log.Printf("tx.Rollback() error: %s\n", rbErr.Error())
			}
		} else if err := tx.Commit(); err != nil {
			log.Printf("tx.Commit() error: %s\n", err.Error())
			_ = tx.Rollback()
		}

		timeElapsed := stratuxClock.Since(timeStart)
		if debug && timeElapsed.Seconds() > 0 {
			rowsPerSecond := float64(nRows) / timeElapsed.Seconds()
			log.Printf("Writing finished. %d rows in %.2f seconds (%.1f rows per second).\n", nRows, timeElapsed.Seconds(), rowsPerSecond)
		}
		if timeElapsed.Seconds() > 10.0 {
			log.Printf("WARNING! SQLite logging is behind. Last write took %.1f seconds.\n", timeElapsed.Seconds())
			dataLogCriticalErr := fmt.Errorf("WARNING! SQLite logging is behind. Last write took %.1f seconds.", timeElapsed.Seconds())
			addSystemError(dataLogCriticalErr)
		}
	}

	for {
		select {
		case r, ok := <-writeChan:
			if !ok {
				log.Printf("datalog.go: dataLogWriter() received shutdown with rowsQueuedForWrite = %d\n", len(rowsQueuedForWrite))
				flushRows(rowsQueuedForWrite)
				log.Printf("datalog.go: dataLogWriter() shutting down\n")
				return
			}
			rowsQueuedForWrite = append(rowsQueuedForWrite, r)
		case <-writeTicker.C:
			flushRows(rowsQueuedForWrite)
			rowsQueuedForWrite = make([]DataLogRow, 0)
		}
	}
}

func dataLog(stopCh <-chan struct{}, doneCh chan struct{}) {
	log.Printf("datalog.go: dataLog() started\n")

	defer func() {
		dataLogReady.Store(false)
		dataLogMu.Lock()
		dataLogStarted = false
		dataLogStopCh = nil
		dataLogMu.Unlock()
		close(doneCh)
	}()

	now := stratuxClock.GetTime()
	dataLogTimestampsMutex.Lock()
	dataLogTimestamps = map[int64]StratuxTimestamp{
		0: {
			id:                   0,
			Time_type_preference: 0, // stratuxClock.
			StratuxClock_value:   now,
			GPSClock_value:       time.Time{},
			PreferredTime_value:  now,
		},
	}
	dataLogCurTimestamp = 0
	dataLogTimestampsMutex.Unlock()

	db, err := sql.Open("sqlite3", dataLogFilef)
	if err != nil {
		log.Printf("sql.Open(%s): %s\n", dataLogFilef, err.Error())
		return
	}

	defer func() {
		if err := db.Close(); err != nil {
			log.Printf("datalog.go: db.Close() error: %s\n", err.Error())
		}
		log.Printf("datalog.go: dataLog() has closed DB in %s\n", dataLogFilef)
	}()

	// Single connection ensures PRAGMAs apply consistently and prevents SQLITE_BUSY across pool conns.
	db.SetMaxOpenConns(1)

	if _, err = db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		log.Printf("db.Exec('PRAGMA busy_timeout=5000') err: %s\n", err.Error())
	}
	if _, err = db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		log.Printf("db.Exec('PRAGMA journal_mode=WAL') err: %s\n", err.Error())
	}
	if _, err = db.Exec("PRAGMA synchronous=NORMAL"); err != nil {
		log.Printf("db.Exec('PRAGMA synchronous=NORMAL') err: %s\n", err.Error())
	}

	// Ensure all required tables exist (including pong_update and ais_message).
	makeTable(StratuxTimestamp{}, "timestamp", db)
	makeTable(SituationData{}, "mySituation", db)
	makeTable(status{}, "status", db)
	makeTable(settings{}, "settings", db)
	makeTable(TrafficInfo{}, "traffic", db)
	makeTable(msg{}, "messages", db)
	makeTable(esmsg{}, "es_messages", db)
	makeTable(Dump1090TermMessage{}, "dump1090_terminal", db)
	makeTable(PongTermMessage{}, "pong_update", db)
	makeTable(AISTermMessage{}, "ais_message", db)
	makeTable(gpsPerfStats{}, "gps_attitude", db)
	makeTable(StratuxStartup{}, "startup", db)

	// Insert the "startup" entry synchronously before launching dataLogWriter.
	initBatch := newDataLogBatch()
	stratuxStartupID = initBatch.insertData(StratuxStartup{}, "startup", db, 0)

	// Allocate writer channels synchronously before launching dataLogWriter.
	writeChan := make(chan DataLogRow, 10240)
	writerDone := make(chan struct{})
	go dataLogWriter(db, writeChan, writerDone)

	dataLogReady.Store(true)
	for {
		select {
		case r := <-dataLogChan:
			r.ts_num = checkTimestamp()
			writeChan <- r
		case <-stopCh:
			log.Printf("datalog.go: dataLog() received shutdown message\n")
			dataLogReady.Store(false)
			// Drain already-queued entries from dataLogChan before closing writeChan.
		drainLoop:
			for {
				select {
				case r := <-dataLogChan:
					r.ts_num = checkTimestamp()
					writeChan <- r
				default:
					break drainLoop
				}
			}
			close(writeChan)
			<-writerDone
			log.Printf("datalog.go: dataLog() shutting down\n")
			return
		}
	}
}

/*
setDataLogTimeWithGPS().

	Create a timestamp entry using GPS time.
*/
func setDataLogTimeWithGPS(sit SituationData) {
	if !isDataLogReady() || !isGPSClockValid() {
		return
	}
	ts := StratuxTimestamp{
		id:                   0,
		Time_type_preference: 1, // gpsClock.
		StratuxClock_value:   stratuxClock.GetTime(),
		GPSClock_value:       sit.GPSTime,
		PreferredTime_value:  sit.GPSTime,
	}

	dataLogTimestampsMutex.Lock()
	if dataLogTimestamps == nil {
		dataLogTimestamps = make(map[int64]StratuxTimestamp)
	}
	dataLogCurTimestamp++
	dataLogTimestamps[dataLogCurTimestamp] = ts
	pruneOldTimestampsLocked()
	dataLogTimestampsMutex.Unlock()
}

/*
logSituation(), logStatus(), ... pass messages from other functions to the logging

	engine. These are only queued into `dataLogChan` if the Replay Log is toggled on,
	and if the log system is ready to accept writes. Sends are non-blocking so SQLite
	stalls never freeze trafficMutex or RF decoders.
*/
func isDataLogReady() bool {
	return dataLogReady.Load()
}

func enqueueDataLog(tbl string, data any) {
	if !isDataLogReady() {
		return
	}
	select {
	case dataLogChan <- DataLogRow{tbl: tbl, data: data}:
	default:
	}
}

func isReplayLogEnabled() (replay bool, debug bool) {
	globalSettingsMutex.RLock()
	replay = globalSettings.ReplayLog
	debug = globalSettings.DEBUG
	globalSettingsMutex.RUnlock()
	return
}

func logSituation() {
	if replay, _ := isReplayLogEnabled(); replay && isDataLogReady() {
		enqueueDataLog("mySituation", snapshotSituation())
	}
}

func logStatus() {
	if replay, _ := isReplayLogEnabled(); replay && isDataLogReady() {
		enqueueDataLog("status", snapshotStatus())
	}
}

func logSettings() {
	if replay, _ := isReplayLogEnabled(); replay && isDataLogReady() {
		globalSettingsMutex.RLock()
		snap := globalSettings
		globalSettingsMutex.RUnlock()
		enqueueDataLog("settings", snap)
	}
}

func logTraffic(ti TrafficInfo) {
	if replay, _ := isReplayLogEnabled(); replay && isDataLogReady() {
		enqueueDataLog("traffic", ti)
	}
}

func logMsg(m msg) {
	if replay, _ := isReplayLogEnabled(); replay && isDataLogReady() {
		enqueueDataLog("messages", m)
	}
}

func logESMsg(m esmsg) {
	if replay, _ := isReplayLogEnabled(); replay && isDataLogReady() {
		enqueueDataLog("es_messages", m)
	}
}

func logGPSAttitude(gpsPerf gpsPerfStats) {
	if replay, _ := isReplayLogEnabled(); replay && isDataLogReady() {
		enqueueDataLog("gps_attitude", gpsPerf)
	}
}

func logDump1090TermMessage(m Dump1090TermMessage) {
	if replay, debug := isReplayLogEnabled(); debug && replay && isDataLogReady() {
		enqueueDataLog("dump1090_terminal", m)
	}
}

func logPongTermMessage(m PongTermMessage) {
	if replay, debug := isReplayLogEnabled(); debug && replay && isDataLogReady() {
		enqueueDataLog("pong_update", m)
	}
}

func logAISTermMessage(m AISTermMessage) {
	if replay, debug := isReplayLogEnabled(); debug && replay && isDataLogReady() {
		enqueueDataLog("ais_message", m)
	}
}

func initDataLog() {
	go dataLogWatchdog()
}

/*
dataLogWatchdog(): Watchdog function to control startup / shutdown of data logging subsystem.

	Called by initDataLog as a goroutine. It iterates once per second to determine if
	globalSettings.ReplayLog has toggled. If logging was switched from off to on, it starts
	dataLog() as a goroutine. If the log is running and we want it to stop, it calls
	closeDataLog() to turn off the input channels, flush buffered rows, close the log,
	and tear down the dataLog and dataLogWriter goroutines.
*/
func dataLogWatchdog() {
	for {
		replay, _ := isReplayLogEnabled()

		dataLogMu.Lock()
		started := dataLogStarted
		if !started && replay {
			log.Printf("datalog.go: Watchdog wants to START logging.\n")
			dataLogStarted = true
			stopCh := make(chan struct{})
			doneCh := make(chan struct{})
			dataLogStopCh = stopCh
			dataLogDoneCh = doneCh
			dataLogMu.Unlock()
			go dataLog(stopCh, doneCh)
		} else {
			dataLogMu.Unlock()
			if started && !replay {
				log.Printf("datalog.go: Watchdog wants to STOP logging.\n")
				closeDataLog()
			}
		}
		time.Sleep(1 * time.Second)
	}
}

/*
closeDataLog(): Handler for graceful shutdown of data logging goroutines. Safe against

	concurrent calls (e.g. dataLogWatchdog and gracefulShutdown).
*/
func closeDataLog() {
	dataLogReady.Store(false)

	dataLogMu.Lock()
	if !dataLogStarted {
		dataLogMu.Unlock()
		return
	}
	stopCh := dataLogStopCh
	doneCh := dataLogDoneCh
	dataLogStopCh = nil // Ensure stopCh is closed at most once.
	dataLogMu.Unlock()

	if stopCh != nil {
		log.Printf("datalog.go: Starting data log shutdown\n")
		close(stopCh)
	}
	if doneCh != nil {
		log.Printf("datalog.go: Waiting for shutdown signal from dataLog()\n")
		<-doneCh
	}
	log.Printf("datalog.go: Data log shutdown successful.\n")
}
