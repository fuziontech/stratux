package common

import (
	"context"
	"os"
	"strconv"
	"strings"
	"time"
)

const InvalidCpuTemp = float32(-99.0)

const cpuTempSysfsPath = "/sys/class/thermal/thermal_zone0/temp"

type CpuTempUpdateFunc func(cpuTemp float32)

func parseCPUTemp(raw []byte) float32 {
	tempStr := strings.TrimSpace(string(raw))
	if tempStr == "" {
		return InvalidCpuTemp
	}
	tInt, err := strconv.Atoi(tempStr)
	if err != nil {
		return InvalidCpuTemp
	}
	if tInt > 1000 {
		return float32(tInt) / float32(1000.0)
	}
	return float32(tInt) // case where Temp is returned as simple integer
}

func readCPUTemp(path string) float32 {
	temp, err := os.ReadFile(path)
	if err != nil {
		return InvalidCpuTemp
	}
	return parseCPUTemp(temp)
}

func cpuTempMonitor(ctx context.Context, path string, interval time.Duration, updater CpuTempUpdateFunc) {
	if updater == nil {
		return
	}
	timer := time.NewTicker(interval)
	defer timer.Stop()

	for {
		// Update CPUTemp.
		t := readCPUTemp(path)
		if IsCPUTempValid(t) { // Only update if valid value was obtained.
			updater(t)
		}
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}

/* CpuTempMonitor reads the RPi board temperature every second and
calls a callback.  This is broken out into its own function (run as
its own goroutine) because the RPi temperature monitor code is buggy,
and often times reading this file hangs quite some time.  */

func CpuTempMonitor(updater CpuTempUpdateFunc) {
	cpuTempMonitor(context.Background(), cpuTempSysfsPath, 1*time.Second, updater)
}

// Check if CPU temperature is valid. Assume <= 0 is invalid.
func IsCPUTempValid(cpuTemp float32) bool {
	return cpuTemp > 0
}
