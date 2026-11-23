package main

import (
	"bytes"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"encoding/xml"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type testFlightRow struct {
	ts         time.Time
	lat        float64
	lon        float64
	altMSL     float64
	baroAlt    float64
	speedKt    float64
	trackDeg   float64
	pitchDeg   float64
	rollDeg    float64
	fixQuality int
}

func buildTestFlightDB(t *testing.T, tableName string, rows []testFlightRow) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test_stratux.sqlite")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	// Use datalog.go's exact schema builders so column compatibility is verified continuously.
	makeTable(StratuxTimestamp{}, "timestamp", db)
	makeTable(SituationData{}, tableName, db)

	for _, r := range rows {
		res, err := db.Exec(
			`INSERT INTO "timestamp" ("Time_type_preference", "StratuxClock_value", "GPSClock_value", "PreferredTime_value", "StartupID") VALUES (?, ?, ?, ?, ?)`,
			1,
			r.ts.UTC().String(),
			r.ts.UTC().String(),
			r.ts.UTC().String(),
			1,
		)
		if err != nil {
			t.Fatalf("insert timestamp: %v", err)
		}
		tsID, err := res.LastInsertId()
		if err != nil {
			t.Fatalf("LastInsertId: %v", err)
		}

		q := `INSERT INTO ` + quoteSQLIdent(tableName) + ` (
			"GPSLatitude", "GPSLongitude", "GPSFixQuality", "GPSAltitudeMSL",
			"GPSGroundSpeed", "GPSTrueCourse", "GPSTime",
			"BaroPressureAltitude", "AHRSPitch", "AHRSRoll", "AHRSGyroHeading", "timestamp_id"
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
		if _, err := db.Exec(
			q,
			r.lat, r.lon, r.fixQuality, r.altMSL,
			r.speedKt, r.trackDeg, r.ts.UTC().String(),
			r.baroAlt, r.pitchDeg, r.rollDeg, r.trackDeg, tsID,
		); err != nil {
			t.Fatalf("insert %s: %v", tableName, err)
		}
	}

	return dbPath
}

func sampleTwoFlights() []testFlightRow {
	t0 := time.Date(2026, 7, 15, 14, 0, 0, 0, time.UTC)
	tFlight2 := t0.Add(22 * time.Minute) // > 10 minute gap (20 min after flight 1 end at +2m)
	tNoise := tFlight2.Add(25 * time.Minute)

	return []testFlightRow{
		// Invalid fix quality row (must be skipped).
		{ts: t0.Add(-10 * time.Second), lat: 37.5, lon: -122.0, altMSL: 100, fixQuality: 0},
		// Flight 1: 3 points in Northern/Western hemisphere (2 minutes, ~4.0 NM).
		{ts: t0, lat: 37.5000, lon: -122.0000, altMSL: 1000, baroAlt: 980, speedKt: 85, trackDeg: 0, pitchDeg: 3.5, rollDeg: 0.5, fixQuality: 1},
		{ts: t0.Add(1 * time.Minute), lat: 37.5333, lon: -122.0000, altMSL: 3500, baroAlt: 3450, speedKt: 112, trackDeg: 0, pitchDeg: 1.0, rollDeg: -2.0, fixQuality: 2},
		{ts: t0.Add(2 * time.Minute), lat: 37.5666, lon: -122.0000, altMSL: 2500, baroAlt: 2480, speedKt: 95, trackDeg: 0, pitchDeg: -2.0, rollDeg: 0.0, fixQuality: 2},
		// Flight 2 (20-minute gap): 3 points in Southern/Western hemisphere (verifies S/W IGC signs).
		{ts: tFlight2, lat: -33.5000, lon: -70.6000, altMSL: 2000, baroAlt: 1950, speedKt: 70, trackDeg: 180, pitchDeg: 2.0, rollDeg: 1.5, fixQuality: 1},
		{ts: tFlight2.Add(1 * time.Minute), lat: -33.5500, lon: -70.6000, altMSL: 5000, baroAlt: 4920, speedKt: 130, trackDeg: 180, pitchDeg: 0.5, rollDeg: 0.0, fixQuality: 1},
		{ts: tFlight2.Add(2 * time.Minute), lat: -33.6000, lon: -70.6000, altMSL: 4200, baroAlt: 4180, speedKt: 110, trackDeg: 180, pitchDeg: -1.5, rollDeg: -3.0, fixQuality: 1},
		// Single isolated noise point after another > 10m gap (must be filtered out because < 2 points).
		{ts: tNoise, lat: -33.7000, lon: -70.7000, altMSL: 1500, baroAlt: 1500, speedKt: 0, fixQuality: 1},
	}
}

func TestListAndLoadFlightsFromDB(t *testing.T) {
	// Test against both "mySituation" (exact datalog.go table) and "situation" fallback.
	for _, tbl := range []string{"mySituation", "situation"} {
		t.Run(tbl, func(t *testing.T) {
			dbPath := buildTestFlightDB(t, tbl, sampleTwoFlights())

			flights, err := ListFlightsFromDB(dbPath)
			if err != nil {
				t.Fatalf("ListFlightsFromDB: %v", err)
			}
			if len(flights) != 2 {
				t.Fatalf("len(flights) = %d, want 2: %+v", len(flights), flights)
			}

			f1 := flights[0]
			if f1.ID != 1 || f1.PointCount != 3 || f1.DurationSec != 120 {
				t.Fatalf("flight 1 metadata unexpected: %+v", f1)
			}
			if math.Abs(f1.MaxAltFt-3500) > 0.1 || math.Abs(f1.MaxSpeedKt-112) > 0.1 {
				t.Fatalf("flight 1 max alt/speed unexpected: %+v", f1)
			}
			// 0.0666 deg latitude ≈ 4.0 NM.
			if f1.DistanceNM < 3.5 || f1.DistanceNM > 4.5 {
				t.Fatalf("flight 1 DistanceNM = %.2f, want ~4.0 NM", f1.DistanceNM)
			}

			f2 := flights[1]
			if f2.ID != 2 || f2.PointCount != 3 || f2.DurationSec != 120 {
				t.Fatalf("flight 2 metadata unexpected: %+v", f2)
			}
			if math.Abs(f2.MaxAltFt-5000) > 0.1 || math.Abs(f2.MaxSpeedKt-130) > 0.1 {
				t.Fatalf("flight 2 max alt/speed unexpected: %+v", f2)
			}
			// 0.10 deg latitude ≈ 6.0 NM.
			if f2.DistanceNM < 5.5 || f2.DistanceNM > 6.5 {
				t.Fatalf("flight 2 DistanceNM = %.2f, want ~6.0 NM", f2.DistanceNM)
			}

			summary, pts, err := LoadFlightTrackFromDB(dbPath, 2)
			if err != nil {
				t.Fatalf("LoadFlightTrackFromDB(2): %v", err)
			}
			if summary.ID != 2 || len(pts) != 3 {
				t.Fatalf("LoadFlightTrackFromDB(2) got id=%d pts=%d", summary.ID, len(pts))
			}

			if _, _, err := LoadFlightTrackFromDB(dbPath, 99); !errors.Is(err, ErrFlightNotFound) {
				t.Fatalf("expected ErrFlightNotFound for flight 99, got %v", err)
			}
		})
	}
}

func TestWriteFlightIGC(t *testing.T) {
	dbPath := buildTestFlightDB(t, "mySituation", sampleTwoFlights())
	summary, pts, err := LoadFlightTrackFromDB(dbPath, 2) // S/W hemisphere flight
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := WriteFlightIGC(&buf, summary, pts); err != nil {
		t.Fatalf("WriteFlightIGC: %v", err)
	}

	raw := buf.String()
	if !strings.Contains(raw, "ASTR001Stratux\r\n") {
		t.Errorf("missing A-record header in IGC:\n%s", raw)
	}
	if !strings.Contains(raw, "HFDTE150726\r\n") {
		t.Errorf("missing HFDTE150726 in IGC:\n%s", raw)
	}
	if !strings.Contains(raw, "HFFXA035\r\n") || !strings.Contains(raw, "HFPLTPILOTINCHARGE:\r\n") || !strings.Contains(raw, "HFDTM100GPSDATUM:WGS-1984\r\n") {
		t.Errorf("missing required H-records in IGC:\n%s", raw)
	}

	lines := strings.Split(strings.TrimSuffix(raw, "\r\n"), "\r\n")
	bRecords := 0
	for _, line := range lines {
		if strings.HasPrefix(line, "B") {
			bRecords++
			if len(line) != 35 {
				t.Errorf("IGC B-record length = %d, want 35: %q", len(line), line)
			}
			// Verify Southern/Western hemisphere characters at indices 14 ('S') and 23 ('W') and fix validity 'A' at 24.
			if line[14] != 'S' || line[23] != 'W' || line[24] != 'A' {
				t.Errorf("IGC B-record hemisphere/fix invalid: %q (latH=%c lonH=%c fix=%c)", line, line[14], line[23], line[24])
			}
		}
	}
	if bRecords != len(pts) {
		t.Fatalf("IGC B-record count = %d, want %d", bRecords, len(pts))
	}

	// Verify exact first B-record: 14:22:00 UTC, -33.5000 (3330000S), -70.6000 (07036000W), baro=1950ft(594m), gnss=2000ft(610m).
	wantFirstB := "B1422003330000S07036000WA0059400610"
	if !strings.Contains(raw, wantFirstB+"\r\n") {
		t.Errorf("expected first B-record %q in IGC output:\n%s", wantFirstB, raw)
	}
}

func TestWriteFlightGPX(t *testing.T) {
	dbPath := buildTestFlightDB(t, "mySituation", sampleTwoFlights())
	summary, pts, err := LoadFlightTrackFromDB(dbPath, 1)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := WriteFlightGPX(&buf, summary, pts); err != nil {
		t.Fatalf("WriteFlightGPX: %v", err)
	}

	var parsed struct {
		XMLName xml.Name `xml:"gpx"`
		Version string   `xml:"version,attr"`
		Trk     struct {
			Name   string `xml:"name"`
			TrkSeg struct {
				TrkPts []struct {
					Lat  float64 `xml:"lat,attr"`
					Lon  float64 `xml:"lon,attr"`
					Ele  float64 `xml:"ele"`
					Time string  `xml:"time"`
				} `xml:"trkpt"`
			} `xml:"trkseg"`
		} `xml:"trk"`
	}
	if err := xml.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("xml.Unmarshal GPX failed: %v\n%s", err, buf.String())
	}
	if parsed.Version != "1.1" {
		t.Errorf("GPX version = %q, want 1.1", parsed.Version)
	}
	if len(parsed.Trk.TrkSeg.TrkPts) != len(pts) {
		t.Fatalf("GPX trkpt count = %d, want %d", len(parsed.Trk.TrkSeg.TrkPts), len(pts))
	}
	if math.Abs(parsed.Trk.TrkSeg.TrkPts[0].Lat-37.5) > 1e-4 || math.Abs(parsed.Trk.TrkSeg.TrkPts[0].Lon-(-122.0)) > 1e-4 {
		t.Errorf("GPX first trkpt coords = (%f, %f)", parsed.Trk.TrkSeg.TrkPts[0].Lat, parsed.Trk.TrkSeg.TrkPts[0].Lon)
	}
}

func TestWriteFlightKML(t *testing.T) {
	dbPath := buildTestFlightDB(t, "mySituation", sampleTwoFlights())
	summary, pts, err := LoadFlightTrackFromDB(dbPath, 1)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := WriteFlightKML(&buf, summary, pts); err != nil {
		t.Fatalf("WriteFlightKML: %v", err)
	}

	var parsed struct {
		XMLName  xml.Name `xml:"kml"`
		Document struct {
			Name      string `xml:"name"`
			Placemark struct {
				LineString struct {
					Extrude      int    `xml:"extrude"`
					AltitudeMode string `xml:"altitudeMode"`
					Coordinates  string `xml:"coordinates"`
				} `xml:"LineString"`
			} `xml:"Placemark"`
		} `xml:"Document"`
	}
	if err := xml.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("xml.Unmarshal KML failed: %v\n%s", err, buf.String())
	}
	ls := parsed.Document.Placemark.LineString
	if ls.Extrude != 1 || ls.AltitudeMode != "absolute" {
		t.Errorf("KML LineString extrude=%d altitudeMode=%q, want 1 / absolute", ls.Extrude, ls.AltitudeMode)
	}
	coordTokens := strings.Fields(ls.Coordinates)
	if len(coordTokens) != len(pts) {
		t.Fatalf("KML coordinates count = %d, want %d", len(coordTokens), len(pts))
	}
	if !strings.HasPrefix(coordTokens[0], "-122.000000,37.500000,") {
		t.Errorf("KML first coordinate = %q, want lon,lat,alt starting with -122.000000,37.500000,", coordTokens[0])
	}
}

func TestWriteFlightCSV(t *testing.T) {
	dbPath := buildTestFlightDB(t, "mySituation", sampleTwoFlights())
	summary, pts, err := LoadFlightTrackFromDB(dbPath, 1)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := WriteFlightCSV(&buf, summary, pts); err != nil {
		t.Fatalf("WriteFlightCSV: %v", err)
	}

	records, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatalf("csv.ReadAll failed: %v", err)
	}
	if len(records) != len(pts)+1 {
		t.Fatalf("CSV row count = %d, want %d (1 header + %d points)", len(records), len(pts)+1, len(pts))
	}
	if _, err := time.Parse(time.RFC3339, records[1][0]); err != nil {
		t.Errorf("CSV timestamp %q is not valid RFC3339: %v", records[1][0], err)
	}
}

func TestFlightHTTPHandlers(t *testing.T) {
	dbPath := buildTestFlightDB(t, "mySituation", sampleTwoFlights())
	origLogFile := dataLogFilef
	dataLogFilef = dbPath
	defer func() { dataLogFilef = origLogFile }()

	// GET /getFlights
	rec := httptest.NewRecorder()
	handleGetFlights(rec, httptest.NewRequest(http.MethodGet, "/getFlights", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /getFlights status = %d, want 200", rec.Code)
	}
	var summaries []FlightSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &summaries); err != nil {
		t.Fatalf("unmarshal /getFlights: %v", err)
	}
	if len(summaries) != 2 {
		t.Fatalf("GET /getFlights returned %d flights, want 2", len(summaries))
	}

	// GET /getFlightTrack?id=1
	rec = httptest.NewRecorder()
	handleGetFlightTrack(rec, httptest.NewRequest(http.MethodGet, "/getFlightTrack?id=1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /getFlightTrack?id=1 status = %d, want 200", rec.Code)
	}
	var trackResp FlightTrackResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &trackResp); err != nil {
		t.Fatalf("unmarshal /getFlightTrack: %v", err)
	}
	if trackResp.Summary.ID != 1 || len(trackResp.Points) != 3 {
		t.Fatalf("unexpected /getFlightTrack payload: %+v", trackResp)
	}

	// GET /exportFlight for all 4 formats.
	for _, fm := range []string{"igc", "gpx", "kml", "csv"} {
		rec = httptest.NewRecorder()
		handleExportFlight(rec, httptest.NewRequest(http.MethodGet, "/exportFlight?id=1&format="+fm, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /exportFlight format=%s status=%d, want 200", fm, rec.Code)
		}
		disp := rec.Header().Get("Content-Disposition")
		wantDisp := `attachment; filename="stratux-flight-1.` + fm + `"`
		if disp != wantDisp {
			t.Errorf("format=%s Content-Disposition=%q, want %q", fm, disp, wantDisp)
		}
		if rec.Body.Len() == 0 {
			t.Errorf("format=%s returned empty body", fm)
		}
	}

	// Invalid format -> 400.
	rec = httptest.NewRecorder()
	handleExportFlight(rec, httptest.NewRequest(http.MethodGet, "/exportFlight?id=1&format=exe", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid format status = %d, want 400", rec.Code)
	}

	// Non-existent flight -> 404.
	rec = httptest.NewRecorder()
	handleExportFlight(rec, httptest.NewRequest(http.MethodGet, "/exportFlight?id=99&format=igc", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing flight status = %d, want 404", rec.Code)
	}
}
