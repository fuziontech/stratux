package main

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/stratux/stratux/uatparse"
)

type replaySummary struct {
	File                 string         `json:"file"`
	DurationMs           float64        `json:"duration_ms"`
	LinesTotal           int            `json:"lines_total"`
	UplinksSeen          int            `json:"uplinks_seen"`
	UplinksParsed        int            `json:"uplinks_parsed"`
	UplinksFailed        int            `json:"uplinks_failed"`
	DownlinksSeen        int            `json:"downlinks_seen"`
	DownlinksValid       int            `json:"downlinks_valid"`
	DownlinksInvalid     int            `json:"downlinks_invalid"`
	InfoFramesTotal      int            `json:"info_frames_total"`
	TextReports          int            `json:"text_reports"`
	NEXRADBlocks         int            `json:"nexrad_blocks"`
	NEXRADCONUSBlocks    int            `json:"nexrad_conus_blocks"`
	NEXRADRegionalBlocks int            `json:"nexrad_regional_blocks"`
	GraphicalFrames      int            `json:"graphical_frames"`
	ProductCounts        map[uint32]int `json:"product_counts"`
	ValidationErrors     int            `json:"validation_errors"`
	Panics               int            `json:"panics"`
	MessagesPerSec       float64        `json:"messages_per_sec"`
	AllocBytes           uint64         `json:"alloc_bytes"`
	Mallocs              uint64         `json:"mallocs"`
	HeapAllocBytes       uint64         `json:"heap_alloc_bytes"`
	Passed               bool           `json:"passed"`
}

func isValidLatLon(lat, lon float64) bool {
	if math.IsNaN(lat) || math.IsInf(lat, 0) || math.IsNaN(lon) || math.IsInf(lon, 0) {
		return false
	}
	return lat >= -90.0 && lat <= 90.0 && lon >= -180.0 && lon <= 180.0
}

func extractUATLine(raw string) string {
	line := strings.TrimSpace(raw)
	if line == "" {
		return ""
	}
	if line[0] == '+' || line[0] == '-' {
		return line
	}
	// Support CSV-timestamped UAT logs (e.g. "timestamp,+hex...;").
	if idx := strings.IndexByte(line, ','); idx >= 0 && idx+1 < len(line) {
		rest := strings.TrimSpace(line[idx+1:])
		if len(rest) > 0 && (rest[0] == '+' || rest[0] == '-') {
			return rest
		}
	}
	return line
}

func validateDownlink(line string) error {
	semi := strings.IndexByte(line, ';')
	if semi < 0 {
		return fmt.Errorf("missing ';' delimiter")
	}
	payloadHex := line[1:semi]
	if len(payloadHex) == 0 || len(payloadHex)%2 != 0 {
		return fmt.Errorf("invalid downlink hex length %d", len(payloadHex))
	}
	raw, err := hex.DecodeString(payloadHex)
	if err != nil {
		return fmt.Errorf("invalid downlink hex: %w", err)
	}
	if len(raw) != 18 && len(raw) != 34 {
		return fmt.Errorf("unexpected downlink payload size %d bytes", len(raw))
	}
	return nil
}

func processLineSafe(line string, sum *replaySummary, errSamples *[]string) {
	defer func() {
		if r := recover(); r != nil {
			sum.Panics++
			if len(*errSamples) < 10 {
				*errSamples = append(*errSamples, fmt.Sprintf("panic on line %d: %v", sum.LinesTotal, r))
			}
		}
	}()

	if line == "" {
		return
	}

	switch line[0] {
	case '-':
		sum.DownlinksSeen++
		// Verify uatparse.New handles downlink lines gracefully without panicking.
		_, _ = uatparse.New(line)
		if err := validateDownlink(line); err != nil {
			sum.DownlinksInvalid++
			sum.ValidationErrors++
			if len(*errSamples) < 10 {
				*errSamples = append(*errSamples, fmt.Sprintf("line %d downlink error: %v", sum.LinesTotal, err))
			}
		} else {
			sum.DownlinksValid++
		}
		return

	case '+':
		sum.UplinksSeen++
		msg, err := uatparse.New(line)
		if err != nil {
			sum.UplinksFailed++
			if len(*errSamples) < 10 {
				*errSamples = append(*errSamples, fmt.Sprintf("line %d uplink parse error: %v", sum.LinesTotal, err))
			}
			return
		}
		if err := msg.DecodeUplink(); err != nil {
			sum.UplinksFailed++
			if len(*errSamples) < 10 {
				*errSamples = append(*errSamples, fmt.Sprintf("line %d uplink decode error: %v", sum.LinesTotal, err))
			}
			return
		}
		sum.UplinksParsed++

		if !isValidLatLon(msg.Lat, msg.Lon) {
			sum.ValidationErrors++
			if len(*errSamples) < 10 {
				*errSamples = append(*errSamples, fmt.Sprintf("line %d invalid uplink station lat/lon: (%v, %v)", sum.LinesTotal, msg.Lat, msg.Lon))
			}
		}

		reports, err := msg.GetTextReports()
		if err != nil {
			sum.ValidationErrors++
			if len(*errSamples) < 10 {
				*errSamples = append(*errSamples, fmt.Sprintf("line %d GetTextReports error: %v", sum.LinesTotal, err))
			}
		} else {
			sum.TextReports += len(reports)
		}

		for _, frame := range msg.Frames {
			sum.InfoFramesTotal++
			if frame.Frame_type == 0 {
				sum.ProductCounts[frame.Product_id]++
			}
			switch frame.Product_id {
			case 63, 64:
				for _, blk := range frame.NEXRAD {
					sum.NEXRADBlocks++
					if blk.Radar_Type == 63 {
						sum.NEXRADCONUSBlocks++
					} else if blk.Radar_Type == 64 {
						sum.NEXRADRegionalBlocks++
					}
					if !isValidLatLon(blk.LatNorth, blk.LonWest) || blk.Height <= 0 || blk.Width <= 0 ||
						len(blk.Intensity) == 0 || len(blk.Intensity) > uatparse.MAX_NEXRAD_BINS {
						sum.ValidationErrors++
						if len(*errSamples) < 10 {
							*errSamples = append(*errSamples, fmt.Sprintf("line %d invalid NEXRAD block: lat=%v lon=%v h=%v w=%v bins=%d",
								sum.LinesTotal, blk.LatNorth, blk.LonWest, blk.Height, blk.Width, len(blk.Intensity)))
						}
					}
					for _, v := range blk.Intensity {
						if v > 7 {
							sum.ValidationErrors++
							if len(*errSamples) < 10 {
								*errSamples = append(*errSamples, fmt.Sprintf("line %d NEXRAD intensity out of range: %d", sum.LinesTotal, v))
							}
							break
						}
					}
				}
			case 8, 11, 13, 14, 15, 16, 17:
				sum.GraphicalFrames++
				for _, pt := range frame.Points {
					if !isValidLatLon(pt.Lat, pt.Lon) {
						sum.ValidationErrors++
						if len(*errSamples) < 10 {
							*errSamples = append(*errSamples, fmt.Sprintf("line %d invalid graphical GeoPoint: (%v, %v)", sum.LinesTotal, pt.Lat, pt.Lon))
						}
						break
					}
				}
			}
		}
	}
}

func main() {
	filePath := flag.String("file", "test-data/example.dump978", "Path to dump978 or CSV UAT replay log")
	jsonOut := flag.Bool("json", false, "Emit machine-readable JSON summary only")
	minUplinks := flag.Int("min-uplinks", 1, "Minimum parsed uplinks required to pass")
	minText := flag.Int("min-text", 0, "Minimum decoded FIS-B text reports required to pass")
	minNEXRAD := flag.Int("min-nexrad", 0, "Minimum decoded NEXRAD blocks required to pass")
	flag.Parse()

	f, err := os.Open(*filePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "replay_harness: failed to open %q: %v\n", *filePath, err)
		os.Exit(1)
	}
	defer f.Close()

	sum := replaySummary{
		File:          *filePath,
		ProductCounts: make(map[uint32]int),
	}
	var errSamples []string

	runtime.GC()
	var memBefore, memAfter runtime.MemStats
	runtime.ReadMemStats(&memBefore)
	start := time.Now()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		sum.LinesTotal++
		line := extractUATLine(scanner.Text())
		processLineSafe(line, &sum, &errSamples)
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "replay_harness: error reading %q: %v\n", *filePath, err)
		os.Exit(1)
	}

	elapsed := time.Since(start)
	runtime.ReadMemStats(&memAfter)

	sum.DurationMs = float64(elapsed.Microseconds()) / 1000.0
	if elapsed.Seconds() > 0 {
		sum.MessagesPerSec = float64(sum.UplinksSeen+sum.DownlinksSeen) / elapsed.Seconds()
	}
	if memAfter.TotalAlloc >= memBefore.TotalAlloc {
		sum.AllocBytes = memAfter.TotalAlloc - memBefore.TotalAlloc
	}
	if memAfter.Mallocs >= memBefore.Mallocs {
		sum.Mallocs = memAfter.Mallocs - memBefore.Mallocs
	}
	sum.HeapAllocBytes = memAfter.HeapAlloc

	sum.Passed = sum.Panics == 0 &&
		sum.ValidationErrors == 0 &&
		sum.UplinksFailed == 0 &&
		sum.UplinksParsed >= *minUplinks &&
		sum.TextReports >= *minText &&
		sum.NEXRADBlocks >= *minNEXRAD

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(sum)
	} else {
		fmt.Printf("Replay Harness Summary (%s)\n", sum.File)
		fmt.Printf("  Status:     %s\n", map[bool]string{true: "PASS", false: "FAIL"}[sum.Passed])
		fmt.Printf("  Lines:      %d (uplinks=%d parsed=%d failed=%d, downlinks=%d valid=%d invalid=%d)\n",
			sum.LinesTotal, sum.UplinksSeen, sum.UplinksParsed, sum.UplinksFailed,
			sum.DownlinksSeen, sum.DownlinksValid, sum.DownlinksInvalid)
		fmt.Printf("  FIS-B:      frames=%d text_reports=%d nexrad_blocks=%d (conus=%d regional=%d) graphical_frames=%d\n",
			sum.InfoFramesTotal, sum.TextReports, sum.NEXRADBlocks,
			sum.NEXRADCONUSBlocks, sum.NEXRADRegionalBlocks, sum.GraphicalFrames)
		if len(sum.ProductCounts) > 0 {
			pids := make([]int, 0, len(sum.ProductCounts))
			for pid := range sum.ProductCounts {
				pids = append(pids, int(pid))
			}
			sort.Ints(pids)
			parts := make([]string, 0, len(pids))
			for _, pid := range pids {
				parts = append(parts, fmt.Sprintf("%d:%d", pid, sum.ProductCounts[uint32(pid)]))
			}
			fmt.Printf("  Products:   %s\n", strings.Join(parts, " "))
		}
		fmt.Printf("  Throughput: %.2f ms (%.0f msg/s)\n", sum.DurationMs, sum.MessagesPerSec)
		fmt.Printf("  Memory:     alloc=%d bytes (%d mallocs), heap=%d bytes\n",
			sum.AllocBytes, sum.Mallocs, sum.HeapAllocBytes)
		fmt.Printf("  Safety:     panics=%d validation_errors=%d\n", sum.Panics, sum.ValidationErrors)
		for _, sample := range errSamples {
			fmt.Fprintf(os.Stderr, "  ERROR: %s\n", sample)
		}
	}

	if !sum.Passed {
		os.Exit(1)
	}
}
