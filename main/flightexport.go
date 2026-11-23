/*
	Copyright (c) 2026 Stratux Contributors
	Distributable under the terms of The "BSD New" License
	that can be found in the LICENSE file, herein included
	as part of this header.

	flightexport.go: Automatic flight segmentation from Stratux's SQLite datalog
	and streaming flight track export in IGC, GPX 1.1, KML 2.2, and CSV formats.
*/

package main

import (
	"bytes"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/stratux/stratux/common"
)

const (
	flightGapThreshold = 10 * time.Minute
	minFlightPoints    = 2
	metersPerNM        = 1852.0
	metersPerFoot      = 0.3048
)

// ErrFlightNotFound is returned when a requested flight ID does not exist in the datalog.
var ErrFlightNotFound = errors.New("flight not found")

// FlightSummary describes a segmented flight returned by GET /getFlights.
type FlightSummary struct {
	ID          int       `json:"ID"`
	StartTime   time.Time `json:"StartTime"`
	EndTime     time.Time `json:"EndTime"`
	DurationSec int64     `json:"DurationSec"`
	PointCount  int       `json:"PointCount"`
	DistanceNM  float64   `json:"DistanceNM"`
	MaxAltFt    float64   `json:"MaxAltFt"`
	MaxSpeedKt  float64   `json:"MaxSpeedKt"`
	StartLat    float64   `json:"StartLat"`
	StartLon    float64   `json:"StartLon"`
	EndLat      float64   `json:"EndLat"`
	EndLon      float64   `json:"EndLon"`
}

// FlightTrackPoint represents a single logged GPS/baro/AHRS sample returned by GET /getFlightTrack.
type FlightTrackPoint struct {
	Time      time.Time `json:"Time"`
	Lat       float64   `json:"Lat"`
	Lon       float64   `json:"Lon"`
	AltMSLFt  float64   `json:"AltMSLFt"`
	BaroAltFt float64   `json:"BaroAltFt"`
	SpeedKt   float64   `json:"SpeedKt"`
	TrackDeg  float64   `json:"TrackDeg"`
	PitchDeg  float64   `json:"PitchDeg"`
	RollDeg   float64   `json:"RollDeg"`
}

// FlightTrackResponse is the JSON payload returned by GET /getFlightTrack?id=N.
type FlightTrackResponse struct {
	Summary FlightSummary      `json:"Summary"`
	Points  []FlightTrackPoint `json:"Points"`
}

type segmentedFlight struct {
	summary FlightSummary
	points  []FlightTrackPoint
}

func openSQLiteReadOnly(dbPath string) (*sql.DB, error) {
	if strings.TrimSpace(dbPath) == "" {
		return nil, os.ErrNotExist
	}
	if _, err := os.Stat(dbPath); err != nil {
		return nil, err
	}
	u := &url.URL{
		Scheme:   "file",
		Path:     filepath.ToSlash(dbPath),
		RawQuery: "mode=ro&_busy_timeout=3000",
	}
	db, err := sql.Open("sqlite3", u.String())
	if err != nil {
		return nil, err
	}
	return db, nil
}

// detectSituationTable checks for "mySituation" (the live datalog.go table name) first,
// then falls back to "situation" for legacy or external schemas.
func detectSituationTable(db *sql.DB) (string, error) {
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name IN ('mySituation', 'situation')`)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	hasMySituation := false
	hasSituation := false
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return "", err
		}
		switch name {
		case "mySituation":
			hasMySituation = true
		case "situation":
			hasSituation = true
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if hasMySituation {
		return "mySituation", nil
	}
	if hasSituation {
		return "situation", nil
	}
	return "", fmt.Errorf("no situation table found in datalog database")
}

func tableColumns(db *sql.DB, tbl string) (map[string]bool, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", quoteSQLIdent(tbl)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols := make(map[string]bool)
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			return nil, err
		}
		cols[name] = true
	}
	return cols, rows.Err()
}

func parseStratuxTime(raw any) (time.Time, bool) {
	if raw == nil {
		return time.Time{}, false
	}
	switch v := raw.(type) {
	case time.Time:
		if v.IsZero() {
			return time.Time{}, false
		}
		return v.UTC(), true
	case int64:
		if v <= 0 {
			return time.Time{}, false
		}
		return time.Unix(v, 0).UTC(), true
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
			return time.Time{}, false
		}
		sec, frac := math.Modf(v)
		return time.Unix(int64(sec), int64(frac*1e9)).UTC(), true
	case []byte:
		return parseStratuxTimeString(string(v))
	case string:
		return parseStratuxTimeString(v)
	default:
		return parseStratuxTimeString(fmt.Sprint(v))
	}
}

func parseStratuxTimeString(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	// Strip Go monotonic clock reading suffix (" m=+123.456") produced by time.Time.String().
	if idx := strings.Index(s, " m="); idx >= 0 {
		s = strings.TrimSpace(s[:idx])
	}

	layouts := []string{
		"2006-01-02 15:04:05.999999999 -0700 MST",
		"2006-01-02 15:04:05 -0700 MST",
		"2006-01-02 15:04:05.999999999 -0700 -0700",
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			if t.IsZero() {
				return time.Time{}, false
			}
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

func selectOrZero(cols map[string]bool, alias, col string) string {
	if cols[col] {
		return fmt.Sprintf("COALESCE(%s.%s, 0)", alias, quoteSQLIdent(col))
	}
	return "0"
}

func loadAllTrackPointsFromDB(dbPath string) ([]FlightTrackPoint, error) {
	db, err := openSQLiteReadOnly(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	sitTable, err := detectSituationTable(db)
	if err != nil {
		return nil, err
	}

	sitCols, err := tableColumns(db, sitTable)
	if err != nil {
		return nil, err
	}
	tsCols, _ := tableColumns(db, "timestamp")

	hasTSJoin := sitCols["timestamp_id"] && tsCols["PreferredTime_value"]

	tsExpr := "''"
	joinClause := ""
	if hasTSJoin {
		tsExpr = `t."PreferredTime_value"`
		joinClause = `LEFT JOIN "timestamp" t ON s."timestamp_id" = t."id"`
	}

	gpsTimeExpr := "''"
	if sitCols["GPSTime"] {
		gpsTimeExpr = `COALESCE(s."GPSTime", '')`
	}

	query := fmt.Sprintf(
		`SELECT %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s
		 FROM %s s %s
		 WHERE %s > 0
		 ORDER BY s."id" ASC`,
		tsExpr,
		gpsTimeExpr,
		selectOrZero(sitCols, "s", "GPSLatitude"),
		selectOrZero(sitCols, "s", "GPSLongitude"),
		selectOrZero(sitCols, "s", "GPSAltitudeMSL"),
		selectOrZero(sitCols, "s", "BaroPressureAltitude"),
		selectOrZero(sitCols, "s", "GPSGroundSpeed"),
		selectOrZero(sitCols, "s", "GPSTrueCourse"),
		selectOrZero(sitCols, "s", "AHRSPitch"),
		selectOrZero(sitCols, "s", "AHRSRoll"),
		selectOrZero(sitCols, "s", "GPSFixQuality"),
		quoteSQLIdent(sitTable),
		joinClause,
		selectOrZero(sitCols, "s", "GPSFixQuality"),
	)

	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	points := make([]FlightTrackPoint, 0)
	for rows.Next() {
		var rawPrefTime, rawGPSTime any
		var lat, lon, altMSL, baroAlt, speedKt, trackDeg, pitchDeg, rollDeg float64
		var fixQuality int

		if err := rows.Scan(
			&rawPrefTime,
			&rawGPSTime,
			&lat,
			&lon,
			&altMSL,
			&baroAlt,
			&speedKt,
			&trackDeg,
			&pitchDeg,
			&rollDeg,
			&fixQuality,
		); err != nil {
			return nil, err
		}

		if fixQuality <= 0 {
			continue
		}
		if math.IsNaN(lat) || math.IsInf(lat, 0) || math.IsNaN(lon) || math.IsInf(lon, 0) {
			continue
		}
		if lat < -90 || lat > 90 || lon < -180 || lon > 180 || (lat == 0 && lon == 0) {
			continue
		}

		prefT, prefOK := parseStratuxTime(rawPrefTime)
		gpsT, gpsOK := parseStratuxTime(rawGPSTime)

		var ptTime time.Time
		switch {
		case prefOK && prefT.Year() >= 2000:
			ptTime = prefT
		case gpsOK && gpsT.Year() >= 2000:
			ptTime = gpsT
		case prefOK:
			ptTime = prefT
		case gpsOK:
			ptTime = gpsT
		default:
			continue
		}

		points = append(points, FlightTrackPoint{
			Time:      ptTime.UTC(),
			Lat:       lat,
			Lon:       lon,
			AltMSLFt:  sanitizeFinite(altMSL),
			BaroAltFt: sanitizeFinite(baroAlt),
			SpeedKt:   sanitizeFinite(speedKt),
			TrackDeg:  sanitizeFinite(trackDeg),
			PitchDeg:  sanitizeFinite(pitchDeg),
			RollDeg:   sanitizeFinite(rollDeg),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	sort.SliceStable(points, func(i, j int) bool {
		return points[i].Time.Before(points[j].Time)
	})

	return points, nil
}

func sanitizeFinite(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

func segmentFlights(points []FlightTrackPoint) []segmentedFlight {
	if len(points) < minFlightPoints {
		return nil
	}

	rawSegments := make([][]FlightTrackPoint, 0)
	cur := []FlightTrackPoint{points[0]}

	for i := 1; i < len(points); i++ {
		gap := points[i].Time.Sub(points[i-1].Time)
		if gap > flightGapThreshold || gap < -flightGapThreshold {
			if len(cur) >= minFlightPoints {
				rawSegments = append(rawSegments, cur)
			}
			cur = []FlightTrackPoint{points[i]}
			continue
		}
		cur = append(cur, points[i])
	}
	if len(cur) >= minFlightPoints {
		rawSegments = append(rawSegments, cur)
	}

	flights := make([]segmentedFlight, 0, len(rawSegments))
	for idx, seg := range rawSegments {
		summary := summarizeSegment(idx+1, seg)
		flights = append(flights, segmentedFlight{
			summary: summary,
			points:  seg,
		})
	}
	return flights
}

func summarizeSegment(id int, pts []FlightTrackPoint) FlightSummary {
	first := pts[0]
	last := pts[len(pts)-1]

	durSec := int64(last.Time.Sub(first.Time).Seconds())
	if durSec < 0 {
		durSec = 0
	}

	maxAlt := first.AltMSLFt
	maxSpeed := first.SpeedKt
	if maxSpeed < 0 {
		maxSpeed = 0
	}

	var distMeters float64
	for i := 1; i < len(pts); i++ {
		if pts[i].AltMSLFt > maxAlt {
			maxAlt = pts[i].AltMSLFt
		}
		if pts[i].SpeedKt > maxSpeed {
			maxSpeed = pts[i].SpeedKt
		}
		d, _ := common.Distance(pts[i-1].Lat, pts[i-1].Lon, pts[i].Lat, pts[i].Lon)
		if !math.IsNaN(d) && !math.IsInf(d, 0) && d > 0 {
			distMeters += d
		}
	}

	return FlightSummary{
		ID:          id,
		StartTime:   first.Time.UTC(),
		EndTime:     last.Time.UTC(),
		DurationSec: durSec,
		PointCount:  len(pts),
		DistanceNM:  distMeters / metersPerNM,
		MaxAltFt:    maxAlt,
		MaxSpeedKt:  maxSpeed,
		StartLat:    first.Lat,
		StartLon:    first.Lon,
		EndLat:      last.Lat,
		EndLon:      last.Lon,
	}
}

// ListFlightsFromDB reads the Stratux SQLite datalog read-only and returns summaries
// for all auto-segmented flights (split on time gaps > 10 minutes, filtering < 2 points).
func ListFlightsFromDB(dbPath string) ([]FlightSummary, error) {
	points, err := loadAllTrackPointsFromDB(dbPath)
	if err != nil {
		return nil, err
	}
	flights := segmentFlights(points)
	out := make([]FlightSummary, len(flights))
	for i, f := range flights {
		out[i] = f.summary
	}
	return out, nil
}

// LoadFlightTrackFromDB loads the summary and full track points for a 1-based flightID.
func LoadFlightTrackFromDB(dbPath string, flightID int) (FlightSummary, []FlightTrackPoint, error) {
	if flightID <= 0 {
		return FlightSummary{}, nil, ErrFlightNotFound
	}
	points, err := loadAllTrackPointsFromDB(dbPath)
	if err != nil {
		return FlightSummary{}, nil, err
	}
	flights := segmentFlights(points)
	if flightID > len(flights) {
		return FlightSummary{}, nil, ErrFlightNotFound
	}
	f := flights[flightID-1]
	return f.summary, f.points, nil
}

func formatIGCLat(lat float64) string {
	hemi := 'N'
	if lat < 0 {
		hemi = 'S'
		lat = -lat
	}
	if lat > 90 {
		lat = 90
	}
	totalThousandths := int(math.Round(lat * 60000.0))
	deg := totalThousandths / 60000
	rem := totalThousandths % 60000
	min := rem / 1000
	milli := rem % 1000
	if deg > 90 {
		deg, min, milli = 90, 0, 0
	}
	return fmt.Sprintf("%02d%02d%03d%c", deg, min, milli, hemi)
}

func formatIGCLon(lon float64) string {
	hemi := 'E'
	if lon < 0 {
		hemi = 'W'
		lon = -lon
	}
	if lon > 180 {
		lon = 180
	}
	totalThousandths := int(math.Round(lon * 60000.0))
	deg := totalThousandths / 60000
	rem := totalThousandths % 60000
	min := rem / 1000
	milli := rem % 1000
	if deg > 180 {
		deg, min, milli = 180, 0, 0
	}
	return fmt.Sprintf("%03d%02d%03d%c", deg, min, milli, hemi)
}

func formatIGCAltitudeMeters(altFt float64) string {
	meters := int(math.Round(altFt * metersPerFoot))
	if meters > 99999 {
		meters = 99999
	}
	if meters < -9999 {
		meters = -9999
	}
	if meters < 0 {
		return fmt.Sprintf("-%04d", -meters)
	}
	return fmt.Sprintf("%05d", meters)
}

// WriteFlightIGC writes a valid FAI IGC flight log to w using CRLF line endings.
func WriteFlightIGC(w io.Writer, summary FlightSummary, points []FlightTrackPoint) error {
	startUTC := summary.StartTime.UTC()
	if startUTC.IsZero() && len(points) > 0 {
		startUTC = points[0].Time.UTC()
	}

	headers := []string{
		"ASTR001Stratux",
		"HFDTE" + startUTC.Format("020106"),
		"HFFXA035",
		"HFPLTPILOTINCHARGE:",
		"HFDTM100GPSDATUM:WGS-1984",
		"HFFTYFRTYPE:Stratux",
	}
	for _, line := range headers {
		if _, err := io.WriteString(w, line+"\r\n"); err != nil {
			return err
		}
	}

	for _, pt := range points {
		bRecord := fmt.Sprintf("B%s%s%sA%s%s\r\n",
			pt.Time.UTC().Format("150405"),
			formatIGCLat(pt.Lat),
			formatIGCLon(pt.Lon),
			formatIGCAltitudeMeters(pt.BaroAltFt),
			formatIGCAltitudeMeters(pt.AltMSLFt),
		)
		if _, err := io.WriteString(w, bRecord); err != nil {
			return err
		}
	}
	return nil
}

type gpxDocument struct {
	XMLName  xml.Name     `xml:"gpx"`
	Version  string       `xml:"version,attr"`
	Creator  string       `xml:"creator,attr"`
	Xmlns    string       `xml:"xmlns,attr"`
	Metadata *gpxMetadata `xml:"metadata,omitempty"`
	Trk      gpxTrack     `xml:"trk"`
}

type gpxMetadata struct {
	Name string `xml:"name"`
	Time string `xml:"time,omitempty"`
}

type gpxTrack struct {
	Name   string      `xml:"name"`
	TrkSeg gpxTrackSeg `xml:"trkseg"`
}

type gpxTrackSeg struct {
	TrkPts []gpxTrackPt `xml:"trkpt"`
}

type gpxTrackPt struct {
	Lat  string `xml:"lat,attr"`
	Lon  string `xml:"lon,attr"`
	Ele  string `xml:"ele"`
	Time string `xml:"time"`
}

// WriteFlightGPX writes a valid GPX 1.1 XML track document to w.
func WriteFlightGPX(w io.Writer, summary FlightSummary, points []FlightTrackPoint) error {
	trkPts := make([]gpxTrackPt, 0, len(points))
	for _, pt := range points {
		trkPts = append(trkPts, gpxTrackPt{
			Lat:  strconv.FormatFloat(pt.Lat, 'f', 6, 64),
			Lon:  strconv.FormatFloat(pt.Lon, 'f', 6, 64),
			Ele:  strconv.FormatFloat(pt.AltMSLFt*metersPerFoot, 'f', 1, 64),
			Time: pt.Time.UTC().Format(time.RFC3339),
		})
	}

	name := fmt.Sprintf("Stratux Flight %d", summary.ID)
	var metaTime string
	if !summary.StartTime.IsZero() {
		metaTime = summary.StartTime.UTC().Format(time.RFC3339)
	}

	doc := gpxDocument{
		Version: "1.1",
		Creator: "Stratux",
		Xmlns:   "http://www.topografix.com/GPX/1/1",
		Metadata: &gpxMetadata{
			Name: name,
			Time: metaTime,
		},
		Trk: gpxTrack{
			Name: name,
			TrkSeg: gpxTrackSeg{
				TrkPts: trkPts,
			},
		},
	}

	if _, err := io.WriteString(w, xml.Header); err != nil {
		return err
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\n")
	return err
}

type kmlDocument struct {
	XMLName  xml.Name `xml:"kml"`
	Xmlns    string   `xml:"xmlns,attr"`
	Document kmlBody  `xml:"Document"`
}

type kmlBody struct {
	Name      string       `xml:"name"`
	Style     kmlStyle     `xml:"Style"`
	Placemark kmlPlacemark `xml:"Placemark"`
}

type kmlStyle struct {
	ID        string       `xml:"id,attr"`
	LineStyle kmlLineStyle `xml:"LineStyle"`
	PolyStyle kmlPolyStyle `xml:"PolyStyle"`
}

type kmlLineStyle struct {
	Color string `xml:"color"`
	Width int    `xml:"width"`
}

type kmlPolyStyle struct {
	Color string `xml:"color"`
}

type kmlPlacemark struct {
	Name        string        `xml:"name"`
	Description string        `xml:"description"`
	StyleURL    string        `xml:"styleUrl"`
	LineString  kmlLineString `xml:"LineString"`
}

type kmlLineString struct {
	Extrude      int    `xml:"extrude"`
	Tessellate   int    `xml:"tessellate"`
	AltitudeMode string `xml:"altitudeMode"`
	Coordinates  string `xml:"coordinates"`
}

// WriteFlightKML writes a valid KML 2.2 XML document with an extruded 3D LineString to w.
func WriteFlightKML(w io.Writer, summary FlightSummary, points []FlightTrackPoint) error {
	var coordBuf strings.Builder
	for i, pt := range points {
		if i > 0 {
			coordBuf.WriteByte(' ')
		}
		fmt.Fprintf(&coordBuf, "%.6f,%.6f,%.1f", pt.Lon, pt.Lat, pt.AltMSLFt*metersPerFoot)
	}

	name := fmt.Sprintf("Stratux Flight %d", summary.ID)
	desc := fmt.Sprintf("Duration: %ds | Distance: %.1f NM | Max Alt: %.0f ft | Max Speed: %.0f kt",
		summary.DurationSec, summary.DistanceNM, summary.MaxAltFt, summary.MaxSpeedKt)

	doc := kmlDocument{
		Xmlns: "http://www.opengis.net/kml/2.2",
		Document: kmlBody{
			Name: name,
			Style: kmlStyle{
				ID:        "flightPath",
				LineStyle: kmlLineStyle{Color: "ff00aaff", Width: 3},
				PolyStyle: kmlPolyStyle{Color: "5500aaff"},
			},
			Placemark: kmlPlacemark{
				Name:        name,
				Description: desc,
				StyleURL:    "#flightPath",
				LineString: kmlLineString{
					Extrude:      1,
					Tessellate:   1,
					AltitudeMode: "absolute",
					Coordinates:  coordBuf.String(),
				},
			},
		},
	}

	if _, err := io.WriteString(w, xml.Header); err != nil {
		return err
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\n")
	return err
}

// WriteFlightCSV writes a clean CSV flight track with header and ISO-8601 UTC timestamps to w.
func WriteFlightCSV(w io.Writer, summary FlightSummary, points []FlightTrackPoint) error {
	cw := csv.NewWriter(w)
	header := []string{
		"time_utc",
		"lat",
		"lon",
		"alt_msl_ft",
		"baro_alt_ft",
		"speed_kt",
		"track_deg",
		"pitch_deg",
		"roll_deg",
	}
	if err := cw.Write(header); err != nil {
		return err
	}
	for _, pt := range points {
		row := []string{
			pt.Time.UTC().Format(time.RFC3339),
			strconv.FormatFloat(pt.Lat, 'f', 6, 64),
			strconv.FormatFloat(pt.Lon, 'f', 6, 64),
			strconv.FormatFloat(pt.AltMSLFt, 'f', 1, 64),
			strconv.FormatFloat(pt.BaroAltFt, 'f', 1, 64),
			strconv.FormatFloat(pt.SpeedKt, 'f', 1, 64),
			strconv.FormatFloat(pt.TrackDeg, 'f', 1, 64),
			strconv.FormatFloat(pt.PitchDeg, 'f', 1, 64),
			strconv.FormatFloat(pt.RollDeg, 'f', 1, 64),
		}
		if err := cw.Write(row); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

func effectiveDataLogPath() string {
	if dataLogFilef != "" {
		return dataLogFilef
	}
	return filepath.Join(logDir, dataLogFile)
}

// handleGetFlights serves GET /getFlights returning JSON summaries of segmented flights.
func handleGetFlights(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setPublicJSONHeaders(w)

	flights, err := ListFlightsFromDB(effectiveDataLogPath())
	if err != nil {
		if os.IsNotExist(err) {
			flights = []FlightSummary{}
		} else {
			http.Error(w, fmt.Sprintf("failed to list flights: %s", err.Error()), http.StatusInternalServerError)
			return
		}
	}
	if flights == nil {
		flights = []FlightSummary{}
	}

	out, err := json.Marshal(flights)
	if err != nil {
		http.Error(w, "failed to marshal flights", http.StatusInternalServerError)
		return
	}
	fmt.Fprintf(w, "%s\n", out)
}

// handleGetFlightTrack serves GET /getFlightTrack?id=N returning the summary and track points as JSON.
func handleGetFlightTrack(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setPublicJSONHeaders(w)

	idStr := strings.TrimSpace(r.URL.Query().Get("id"))
	flightID, err := strconv.Atoi(idStr)
	if err != nil || flightID <= 0 {
		http.Error(w, "invalid or missing flight id", http.StatusBadRequest)
		return
	}

	summary, points, err := LoadFlightTrackFromDB(effectiveDataLogPath(), flightID)
	if err != nil {
		if errors.Is(err, ErrFlightNotFound) || os.IsNotExist(err) {
			http.Error(w, "flight not found", http.StatusNotFound)
			return
		}
		http.Error(w, fmt.Sprintf("failed to load flight track: %s", err.Error()), http.StatusInternalServerError)
		return
	}
	if points == nil {
		points = []FlightTrackPoint{}
	}

	resp := FlightTrackResponse{
		Summary: summary,
		Points:  points,
	}
	out, err := json.Marshal(&resp)
	if err != nil {
		http.Error(w, "failed to marshal flight track", http.StatusInternalServerError)
		return
	}
	fmt.Fprintf(w, "%s\n", out)
}

// handleExportFlight serves GET /exportFlight?id=N&format=igc|gpx|kml|csv as a file download.
func handleExportFlight(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimSpace(r.URL.Query().Get("id"))
	flightID, err := strconv.Atoi(idStr)
	if err != nil || flightID <= 0 {
		http.Error(w, "invalid or missing flight id", http.StatusBadRequest)
		return
	}

	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	var contentType string
	var writerFn func(io.Writer, FlightSummary, []FlightTrackPoint) error

	switch format {
	case "igc":
		contentType = "text/plain; charset=utf-8"
		writerFn = WriteFlightIGC
	case "gpx":
		contentType = "application/gpx+xml; charset=utf-8"
		writerFn = WriteFlightGPX
	case "kml":
		contentType = "application/vnd.google-earth.kml+xml; charset=utf-8"
		writerFn = WriteFlightKML
	case "csv":
		contentType = "text/csv; charset=utf-8"
		writerFn = WriteFlightCSV
	default:
		http.Error(w, "unsupported format; expected igc, gpx, kml, or csv", http.StatusBadRequest)
		return
	}

	summary, points, err := LoadFlightTrackFromDB(effectiveDataLogPath(), flightID)
	if err != nil {
		if errors.Is(err, ErrFlightNotFound) || os.IsNotExist(err) {
			http.Error(w, "flight not found", http.StatusNotFound)
			return
		}
		http.Error(w, fmt.Sprintf("failed to load flight: %s", err.Error()), http.StatusInternalServerError)
		return
	}

	var buf bytes.Buffer
	if err := writerFn(&buf, summary, points); err != nil {
		http.Error(w, fmt.Sprintf("failed to render %s export: %s", format, err.Error()), http.StatusInternalServerError)
		return
	}

	setNoCache(w)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="stratux-flight-%d.%s"`, flightID, format))
	_, _ = w.Write(buf.Bytes())
}
