package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/felixge/pidctrl"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/stianeikeland/go-rpio/v4"
	"github.com/stratux/stratux/common"
)

// Initialize Prometheus metrics.
var (
	currentTemp = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "current_temp",
		Help: "Current CPU temp.",
	})

	currentPWM = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "current_pwm",
		Help: "Current PWM Value",
	})

	totalFanOnTime = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "total_fan_on_time",
			Help: "Total fan run time.",
		},
		[]string{"all"},
	)

	totalUptime = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "total_uptime",
			Help: "Total uptime.",
		},
		[]string{"all"},
	)
)

const (
	configLocation = "/boot/firmware/stratux.conf"

	// CPU temperature target, degrees C
	defaultTempTarget = 50.

	/* Minimum duty cycle in % is the point below which the fan  */
	/* stops running nicely from a running situation */
	defaultPwmDutyMin = 0

	/* Maximum duty for PWM controller */
	pwmDutyMax          = 100 // Must be kept at 100
	defaultPwmFrequency = 64000

	// how often to update
	updateDelayMS = 5000

	// start delay of the fan to start the fan to 80% to give the fan a kick to start spinning
	PWMDuty80PStartDelay = 500

	// GPIO-1/BCM "18"/Pin 12 on a Rev 2 and 3,4 Raspberry Pi
	defaultPin = 18

	// name of the service
	name        = "fancontrol"
	description = "cooling fan speed control based on CPU temperature"

	// Address on which daemon should listen (loopback only).
	addr = "127.0.0.1:9977"
)

type FanControl struct {
	TempTarget           float64
	TempCurrent          float64
	PWMDutyMin           uint32
	PWMFrequency         uint32
	PWMDuty80PStartDelay uint32
	PWMDutyCurrent       uint32
	PWMPin               int
}

var (
	fanControlMu   sync.RWMutex
	myFanControl   FanControl
	metricsRegOnce sync.Once
	configChan     = make(chan bool, 1)
	stdlog, errlog *log.Logger
)

func updateStats(quit <-chan struct{}) {
	updateTicker := time.NewTicker(1 * time.Second)
	defer updateTicker.Stop()

	for {
		select {
		case <-quit:
			return
		case <-updateTicker.C:
			fanControlMu.RLock()
			temp := myFanControl.TempCurrent
			duty := myFanControl.PWMDutyCurrent
			fanControlMu.RUnlock()

			totalUptime.With(prometheus.Labels{"all": "all"}).Inc()
			currentTemp.Set(temp)
			currentPWM.Set(float64(duty))
			if duty > 0 {
				totalFanOnTime.With(prometheus.Labels{"all": "all"}).Inc()
			}
		}
	}
}

// Map an incoming range to an outgoing range
func fmap(x, in_min, in_max, out_min, out_max float64) float64 {
	return (x-in_min)*(out_max-out_min)/(in_max-in_min) + out_min
}

func fanControl(quit <-chan struct{}, wg *sync.WaitGroup) {
	defer wg.Done()

	fanControlMu.Lock()
	myFanControl.PWMDuty80PStartDelay = PWMDuty80PStartDelay
	myFanControl.TempCurrent = 0
	myFanControl.PWMDutyCurrent = 0
	pwmPin := myFanControl.PWMPin
	pwmFreq := myFanControl.PWMFrequency
	tempTarget := myFanControl.TempTarget
	fanControlMu.Unlock()

	updateControlDelay := time.NewTicker(updateDelayMS * time.Millisecond)
	defer updateControlDelay.Stop()

	// Monitor Temperature
	go common.CpuTempMonitor(func(cpuTemp float32) {
		if common.IsCPUTempValid(cpuTemp) {
			fanControlMu.Lock()
			myFanControl.TempCurrent = float64(cpuTemp)
			fanControlMu.Unlock()
		}
	})

	// Open Raspberry GPIO pins
	err := rpio.Open()
	if err != nil {
		errlog.Printf("rpio.Open failed: %v\n", err)
		os.Exit(1)
	}
	defer rpio.Close()

	// Set PWM Mode
	pin := rpio.Pin(pwmPin)
	pin.Mode(rpio.Pwm)
	// Default to "ON" (thermal fail-safe) when we bail out; runs before rpio.Close().
	defer pin.DutyCycle(pwmDutyMax, pwmDutyMax)

	setFanFrequency := func(frequency uint32) {
		if frequency < 250000 {
			frequency = 250000
		} else if frequency > 100000000 {
			frequency = 100000000
		}
		pin.Freq(int(frequency))
	}
	setFanFrequency(pwmFreq * 100)

	// Calculate the duty cycle to the hardware value that
	// is appropriate for the HW and minimum fan speed allowed
	dutyCycleToFan := func(dutyCycle float64) float64 {
		fanControlMu.RLock()
		dutyMin := myFanControl.PWMDutyMin
		fanControlMu.RUnlock()
		mappedMinimum := fmap(float64(dutyMin), 0.0, 100.0, 0, float64(pwmDutyMax))
		return fmap(dutyCycle, 0.0, 100.0, mappedMinimum, 100.0)
	}

	// Setup HW to a specific duty cycle 0..100
	setHWDutyCycle := func(value float64) {
		if value < 0.0 {
			value = 0.0
		} else if value > 100.0 {
			value = 100.0
		}
		fanControlMu.Lock()
		myFanControl.PWMDutyCurrent = uint32(value)
		fanControlMu.Unlock()
		pin.DutyCycle(uint32(math.Ceil(fmap(value, 0.0, 100.0, 0.0, float64(pwmDutyMax)))), pwmDutyMax)
	}

	// Fan test function
	turnOnFanTest := func() bool {
		setHWDutyCycle(100.0)
		select {
		case <-quit:
			return false
		case <-time.After(5 * time.Second): // to show user we are running
		}
		fanControlMu.RLock()
		dutyMin := float64(myFanControl.PWMDutyMin)
		fanControlMu.RUnlock()
		setHWDutyCycle(dutyMin)
		select {
		case <-quit:
			return false
		case <-time.After(10 * time.Second):
		}
		return true
	}

	// Power on "test". Allows the user to verify that their fan is working at the selected minimum duty cycle
	// Turns on the fan at minimum duty for 10 seconds. User should see that the fan keeps running all the time
	if !turnOnFanTest() {
		return
	}

	// Start Prometheus
	metricsRegOnce.Do(func() {
		prometheus.MustRegister(currentTemp)
		prometheus.MustRegister(currentPWM)
		prometheus.MustRegister(totalFanOnTime)
		prometheus.MustRegister(totalUptime)
	})
	go updateStats(quit)

	// Create a PID controller
	pidControl := pidctrl.NewPIDController(0.2, 0.2, 0.1)
	pidControl.SetOutputLimits(-100, 0.0)
	pidControl.Set(tempTarget)

	var lastPWMControlValue float64 = 0.0
	lastUpdate := time.Now().Add(-updateDelayMS * time.Millisecond)

	for {
		now := time.Now()
		dt := now.Sub(lastUpdate)
		if dt <= 0 {
			dt = time.Millisecond
		}
		lastUpdate = now

		fanControlMu.RLock()
		tempCurrent := myFanControl.TempCurrent
		fanControlMu.RUnlock()

		// Update the PID controller.
		pidValueOut := -pidControl.UpdateDuration(tempCurrent, dt)

		var fanRequiredDuty float64 // The duty cycle required by the fan
		if pidValueOut > 5.0 || lastPWMControlValue != 0.0 {
			lastPWMControlValue = pidValueOut
			fanRequiredDuty = dutyCycleToFan(pidValueOut)
		} else {
			lastPWMControlValue = 0
			fanRequiredDuty = dutyCycleToFan(1)
		}

		setHWDutyCycle(fanRequiredDuty)

		select {
		case <-quit:
			return
		case <-updateControlDelay.C:
		case <-configChan:
			fanControlMu.RLock()
			freq := myFanControl.PWMFrequency
			target := myFanControl.TempTarget
			fanControlMu.RUnlock()
			setFanFrequency(freq * 100)
			pidControl.Set(target)
		}
	}
}

// Manage by daemon commands or run the daemon
func Run() (string, error) {
	// initialize defaults or from settings
	readSettings()

	// potentially override from command line
	fanControlMu.RLock()
	defaultTemp := myFanControl.TempTarget
	defaultDutyMin := int(myFanControl.PWMDutyMin)
	defaultFreq := int(myFanControl.PWMFrequency)
	defaultPWMPin := myFanControl.PWMPin
	fanControlMu.RUnlock()

	tempTarget := flag.Float64("temp", defaultTemp, "Target CPU Temperature, degrees C")
	dutyMin := flag.Int("minduty", defaultDutyMin, "Minimum PWM duty cycle")
	frequency := flag.Int("frequency", defaultFreq, "PWM Frequency")
	pin := flag.Int("pin", defaultPWMPin, "PWM pin (BCM numbering)")
	flag.Parse()

	fanControlMu.Lock()
	myFanControl.TempTarget = *tempTarget
	myFanControl.PWMDutyMin = uint32(*dutyMin)
	myFanControl.PWMFrequency = uint32(*frequency)
	myFanControl.PWMPin = *pin
	fanControlMu.Unlock()

	quit := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go fanControl(quit, &wg)

	// Set up channel on which to send signal notifications.
	// We must use a buffered channel or risk missing the signal
	// if we're not ready to receive when the signal is sent.
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, syscall.SIGINT, syscall.SIGTERM, syscall.SIGUSR1)

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleStatusRequest)
	mux.Handle("/metrics", promhttp.Handler())
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errlog.Printf("HTTP server error: %v\n", err)
		}
	}()

	// interrupt by system signal
	for {
		killSignal := <-interrupt
		log.Println("Got signal:", killSignal)
		if killSignal == syscall.SIGUSR1 {
			readSettings()
			select {
			case configChan <- true:
			default:
			}
			continue
		}
		close(quit)
		wg.Wait()
		_ = srv.Close()
		if killSignal == syscall.SIGINT {
			return "Daemon was interrupted by system signal", nil
		}
		return "Daemon was killed", nil
	}
}

func readSettingsFromFile(path string) {
	cfg := FanControl{
		PWMDutyMin:           defaultPwmDutyMin,
		TempTarget:           defaultTempTarget,
		PWMFrequency:         defaultPwmFrequency,
		PWMPin:               defaultPin,
		PWMDuty80PStartDelay: PWMDuty80PStartDelay,
	}

	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf("can't read settings %s: %s\n", path, err.Error())
		applyConfigDefaults(cfg)
		return
	}

	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Printf("can't read settings %s: %s\n", path, err.Error())
		applyConfigDefaults(FanControl{
			PWMDutyMin:           defaultPwmDutyMin,
			TempTarget:           defaultTempTarget,
			PWMFrequency:         defaultPwmFrequency,
			PWMPin:               defaultPin,
			PWMDuty80PStartDelay: PWMDuty80PStartDelay,
		})
		return
	}

	applyConfigDefaults(cfg)
	log.Printf("read in settings.\n")
}

func applyConfigDefaults(cfg FanControl) {
	fanControlMu.Lock()
	myFanControl.PWMDutyMin = cfg.PWMDutyMin
	myFanControl.TempTarget = cfg.TempTarget
	myFanControl.PWMFrequency = cfg.PWMFrequency
	myFanControl.PWMPin = cfg.PWMPin
	myFanControl.PWMDuty80PStartDelay = cfg.PWMDuty80PStartDelay
	fanControlMu.Unlock()
}

func readSettings() {
	readSettingsFromFile(configLocation)
}

func handleStatusRequest(w http.ResponseWriter, r *http.Request) {
	fanControlMu.RLock()
	snapshot := myFanControl
	fanControlMu.RUnlock()

	statusJSON, err := json.Marshal(&snapshot)
	if err != nil {
		http.Error(w, "failed to marshal status", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(statusJSON)
}

func init() {
	stdlog = log.New(os.Stdout, "", 0)
	errlog = log.New(os.Stderr, "", 0)
}

func main() {
	status, err := Run()
	if err != nil {
		errlog.Println(status, "\nError: ", err)
	}
	fmt.Println(status)
}
