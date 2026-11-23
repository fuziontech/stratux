/*
	Copyright (c) 2015-2016 Christopher Young
	Distributable under the terms of The "BSD New" License
	that can be found in the LICENSE file, herein included
	as part of this header.

	sdr.go: SDR monitoring, SDR management, data input from UAT/1090ES channels.
*/

package main

import (
	"bufio"
	"context"
	"io"
	"log"
	"math"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	rtl "github.com/jpoirier/gortlsdr"
	"github.com/stratux/stratux/godump978"
)

// Device holds per dongle values and attributes
type Device struct {
	dev       *rtl.Context
	wg        *sync.WaitGroup
	closeCh   chan int
	closeOnce sync.Once
	indexID   int
	ppm       int
	gain      float64
	serial    string
	idSet     bool
}

// UAT is a 978 MHz device
type UAT Device

// ES is a 1090 MHz device
type ES Device

// OGN is an 868 MHz device
type OGN Device

// AIS is an 161 MHz device
type AIS Device

var sdrDevMutex sync.RWMutex

// UATDev holds a 978 MHz dongle object
var UATDev *UAT

// ESDev holds a 1090 MHz dongle object
var ESDev *ES

// OGNDev holds an 868 MHz dongle object
var OGNDev *OGN

// AISDev holds a 162 MHz dongle object
var AISDev *AIS

func hasOGNDev() bool {
	sdrDevMutex.RLock()
	defer sdrDevMutex.RUnlock()
	return OGNDev != nil
}

func hasAISDev() bool {
	sdrDevMutex.RLock()
	defer sdrDevMutex.RUnlock()
	return AISDev != nil
}

type Dump1090TermMessage struct {
	Text   string
	Source string
}

type AISTermMessage struct {
	Text   string
	Source string
}

type subprocessConfig struct {
	name         string
	bin          string
	args         []string
	closeCh      <-chan int
	shutdownFlag *atomic.Bool
	lineMode     bool
	onStdout     func(string)
	onStderr     func(string)
}

var subprocessBackoffs = []time.Duration{
	1 * time.Second,
	2 * time.Second,
	5 * time.Second,
	15 * time.Second,
}

func runSubprocess(cfg subprocessConfig) {
	backoffIdx := 0

	for {
		select {
		case <-cfg.closeCh:
			return
		default:
		}

		ctx, cancel := context.WithCancel(context.Background())
		cmd := exec.CommandContext(ctx, cfg.bin, cfg.args...)

		stdout, err := cmd.StdoutPipe()
		if err != nil {
			cancel()
			log.Printf("%s: Error creating stdout pipe for %s: %s\n", cfg.name, cfg.bin, err)
			cfg.shutdownFlag.Store(true)
			<-cfg.closeCh
			return
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			cancel()
			log.Printf("%s: Error creating stderr pipe for %s: %s\n", cfg.name, cfg.bin, err)
			cfg.shutdownFlag.Store(true)
			<-cfg.closeCh
			return
		}

		startTime := time.Now()
		if err := cmd.Start(); err != nil {
			cancel()
			log.Printf("%s: Error executing %s: %s\n", cfg.name, cfg.bin, err)
			// Don't return immediately, let sdrWatcher drive clean shutdown.
			cfg.shutdownFlag.Store(true)
			<-cfg.closeCh
			return
		}

		log.Printf("%s: Executed %s successfully...\n", cfg.name, cmd.String())

		var autoRestart atomic.Bool
		autoRestart.Store(true)
		procDone := make(chan struct{})

		go func() {
			select {
			case <-procDone:
				return
			case <-cfg.closeCh:
				log.Printf("%s read(): shutdown msg received, stopping subprocess...\n", cfg.name)
				autoRestart.Store(false)
				cancel()
				return
			}
		}()

		var pipeWG sync.WaitGroup
		pipeWG.Add(2)

		readPipe := func(r io.Reader, handler func(string)) {
			defer pipeWG.Done()
			if cfg.lineMode {
				scanner := bufio.NewScanner(r)
				for scanner.Scan() {
					line := strings.TrimSpace(scanner.Text())
					if len(line) > 0 && handler != nil {
						handler(line)
					}
				}
				return
			}
			buf := make([]byte, 1024)
			for {
				n, readErr := r.Read(buf)
				if n > 0 && handler != nil {
					handler(string(buf[:n]))
				}
				if readErr != nil {
					return
				}
			}
		}

		go readPipe(stdout, cfg.onStdout)
		go readPipe(stderr, cfg.onStderr)

		// Go os/exec contract: wait for all pipe reads to finish before cmd.Wait().
		pipeWG.Wait()
		_ = cmd.Wait()
		close(procDone)
		cancel()

		log.Printf("%s: %s terminated...\n", cfg.name, cfg.bin)

		if !autoRestart.Load() || cfg.shutdownFlag.Load() {
			return
		}

		if time.Since(startTime) > 30*time.Second {
			backoffIdx = 0
		}
		waitDur := subprocessBackoffs[backoffIdx]
		if backoffIdx < len(subprocessBackoffs)-1 {
			backoffIdx++
		}

		log.Printf("%s: restarting crashed %s in %s\n", cfg.name, cfg.bin, waitDur)
		timer := time.NewTimer(waitDur)
		select {
		case <-cfg.closeCh:
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

var esStrongSignalRegex = regexp.MustCompile(`(\d+(?:\.\d+)?)%\s*>\s*-3dBFS|(\d+(?:\.\d+)?)%\s+strong`)

func parseESStrongSignalLine(s string) {
	if matches := esStrongSignalRegex.FindStringSubmatch(s); len(matches) >= 3 {
		pctStr := matches[1]
		if pctStr == "" {
			pctStr = matches[2]
		}
		if pct, err := strconv.ParseFloat(pctStr, 64); err == nil {
			hwHealth.RecordESSignalSample(pct)
		}
	}
}

func (e *ES) read() {
	defer e.wg.Done()
	log.Println("Entered ES read() ...")
	// RTL SDR Standard Gains: 0.0 0.9 1.4 2.7 3.7 7.7 8.7 12.5 14.4 15.7 16.6 19.7 20.7 22.9 25.4 28.0 29.7 32.8 33.8 36.4 37.2 38.6 40.2 42.1 43.4 43.9 44.5 48.0 49.6
	if e.gain < 0.9 {
		e.gain = 37.2
	}
	hwHealth.SetSuggestedESGain(e.gain)
	hwHealth.SetBandStatus("es", true, true)
	defer func() {
		globalSettingsMutex.RLock()
		esEn := globalSettings.ES_Enabled || globalSettings.Ping_Enabled || globalSettings.Pong_Enabled
		globalSettingsMutex.RUnlock()
		globalStatusMutex.RLock()
		altConn := globalStatus.Ping_connected || globalStatus.Pong_connected
		globalStatusMutex.RUnlock()
		hwHealth.SetBandStatus("es", esEn, altConn)
	}()
	runSubprocess(subprocessConfig{
		name: "ES",
		bin:  STRATUX_HOME + "/bin/dump1090",
		args: []string{
			"--fix", "--net-stratux-port", "30006", "--net",
			"--device-index", strconv.Itoa(e.indexID),
			"--ppm", strconv.Itoa(e.ppm),
			"--gain", strconv.FormatFloat(e.gain, 'f', -1, 32),
			"--mlat", // display raw messages in Beast ascii mode
		},
		closeCh:      e.closeCh,
		shutdownFlag: &shutdownES,
		onStdout: func(s string) {
			parseESStrongSignalLine(s)
			logDump1090TermMessage(Dump1090TermMessage{Text: s, Source: "stdout"})
		},
		onStderr: func(s string) {
			parseESStrongSignalLine(s)
			logDump1090TermMessage(Dump1090TermMessage{Text: s, Source: "stderr"})
		},
	})
}

func (u *UAT) read() {
	defer u.wg.Done()
	log.Println("Entered UAT read() ...")
	buffer := make([]uint8, rtl.DefaultBufLength)

	for {
		select {
		case <-u.closeCh:
			log.Println("UAT read(): shutdown msg received...")
			return
		default:
		}

		nRead, err := u.dev.ReadSync(buffer, rtl.DefaultBufLength)
		if err != nil {
			if globalSettings.DEBUG {
				log.Printf("\tReadSync Failed - error: %s\n", err)
			}
			shutdownUAT.Store(true)
			<-u.closeCh
			return
		}

		if nRead > 0 {
			// Allocate an owned copy so the next ReadSync never aliases memory read by C.process_data().
			buf := make([]uint8, nRead)
			copy(buf, buffer[:nRead])
			select {
			case godump978.InChan <- buf:
			case <-u.closeCh:
				log.Println("UAT read(): shutdown msg received...")
				return
			}
		}
	}
}

func (f *OGN) read() {
	defer f.wg.Done()
	log.Println("Entered OGN read() ...")

	// ogn-rx doesn't like the time jumping forward while running.. delay initial startup until we have a valid system time
	if !isGPSClockValid() {
		log.Printf("Delaying ogn-rx start until we have a valid GPS time")
		ticker := time.NewTicker(1 * time.Second)
	waitLoop:
		for {
			select {
			case <-f.closeCh:
				ticker.Stop()
				return
			case <-ticker.C:
				if isGPSClockValid() {
					break waitLoop
				}
			}
		}
		ticker.Stop()
	}

	args := []string{"-d", strconv.Itoa(f.indexID), "-p", strconv.Itoa(f.ppm), "-L/var/log/"}
	if !globalSettings.OGNI2CTXEnabled {
		args = append(args, "-t", "off")
	}

	runSubprocess(subprocessConfig{
		name:         "OGN",
		bin:          STRATUX_HOME + "/bin/ogn-rx-eu",
		args:         args,
		closeCh:      f.closeCh,
		shutdownFlag: &shutdownOGN,
		lineMode:     true,
		onStdout: func(line string) {
			log.Println("OGN: ogn-rx-eu stdout: ", line)
		},
		onStderr: func(line string) {
			log.Println("OGN: ogn-rx-eu stderr: ", line)
		},
	})
}

func (e *AIS) read() {
	defer e.wg.Done()
	log.Println("Entered AIS read() ...")
	runSubprocess(subprocessConfig{
		name:         "AIS",
		bin:          STRATUX_HOME + "/bin/rtl_ais",
		args:         []string{"-T", "-k", "-p", strconv.Itoa(e.ppm), "-d", strconv.Itoa(e.indexID)},
		closeCh:      e.closeCh,
		shutdownFlag: &shutdownAIS,
		onStdout: func(s string) {
			logAISTermMessage(AISTermMessage{Text: s, Source: "stdout"})
		},
		onStderr: func(s string) {
			logAISTermMessage(AISTermMessage{Text: s, Source: "stderr"})
		},
	})
}

var ppmRegex = regexp.MustCompile(`str?a?t?u?x:\d+:?(-?\d*)`)

func getPPM(serial string) int {
	arr := ppmRegex.FindStringSubmatch(serial)
	if len(arr) < 2 {
		return globalSettings.PPM
	}

	ppm, err := strconv.Atoi(arr[1])
	if err != nil {
		return globalSettings.PPM
	}

	return ppm
}

func (e *ES) sdrConfig() (err error) {
	e.ppm = getPPM(e.serial)
	globalSettingsMutex.RLock()
	e.gain = globalSettings.Dump1090Gain
	globalSettingsMutex.RUnlock()
	if e.gain > 0 {
		hwHealth.SetSuggestedESGain(e.gain)
	}
	hwHealth.RecordEstimatedPPM("es", e.ppm)
	log.Printf("===== ES Device Serial: %s PPM %d Gain %.1f =====\n", e.serial, e.ppm, e.gain)
	return
}

func (f *OGN) sdrConfig() (err error) {
	f.ppm = getPPM(f.serial)
	hwHealth.RecordEstimatedPPM("ogn", f.ppm)
	log.Printf("===== OGN Device Serial: %s PPM %d =====\n", f.serial, f.ppm)
	return
}

func (f *AIS) sdrConfig() (err error) {
	f.ppm = getPPM(f.serial)
	hwHealth.RecordEstimatedPPM("ais", f.ppm)
	log.Printf("===== AIS Device Serial: %s PPM %d =====\n", f.serial, f.ppm)
	return
}

// 978 UAT configuration settings
const (
	TunerGain    = 480
	SampleRate   = 2083334
	NewRTLFreq   = 28800000
	NewTunerFreq = 28800000
	CenterFreq   = 978000000
	Bandwidth    = 1000000
)

func (u *UAT) sdrConfig() (err error) {
	log.Printf("===== UAT Device Name  : %s =====\n", rtl.GetDeviceName(u.indexID))
	log.Printf("===== UAT Device Serial: %s=====\n", u.serial)

	if u.dev, err = rtl.Open(u.indexID); err != nil {
		log.Printf("\tUAT Open Failed...\n")
		return
	}
	log.Printf("\tGetTunerType: %s\n", u.dev.GetTunerType())

	//---------- Set Tuner Gain ----------
	err = u.dev.SetTunerGainMode(true)
	if err != nil {
		u.dev.Close()
		log.Printf("\tSetTunerGainMode Failed - error: %s\n", err)
		return
	}
	log.Printf("\tSetTunerGainMode Successful\n")

	err = u.dev.SetTunerGain(TunerGain)
	if err != nil {
		u.dev.Close()
		log.Printf("\tSetTunerGain Failed - error: %s\n", err)
		return
	}
	log.Printf("\tSetTunerGain Successful\n")

	tgain := u.dev.GetTunerGain()
	log.Printf("\tGetTunerGain: %d\n", tgain)
	if tgain > 0 {
		u.gain = float64(tgain) / 10.0
	} else {
		u.gain = float64(TunerGain) / 10.0
	}

	//---------- Get/Set Sample Rate ----------
	err = u.dev.SetSampleRate(SampleRate)
	if err != nil {
		u.dev.Close()
		log.Printf("\tSetSampleRate Failed - error: %s\n", err)
		return
	}
	log.Printf("\tSetSampleRate - rate: %d\n", SampleRate)

	log.Printf("\tGetSampleRate: %d\n", u.dev.GetSampleRate())

	//---------- Get/Set Xtal Freq ----------
	rtlFreq, tunerFreq, err := u.dev.GetXtalFreq()
	if err != nil {
		u.dev.Close()
		log.Printf("\tGetXtalFreq Failed - error: %s\n", err)
		return
	}
	log.Printf("\tGetXtalFreq - Rtl: %d, Tuner: %d\n", rtlFreq, tunerFreq)

	err = u.dev.SetXtalFreq(NewRTLFreq, NewTunerFreq)
	if err != nil {
		u.dev.Close()
		log.Printf("\tSetXtalFreq Failed - error: %s\n", err)
		return
	}
	log.Printf("\tSetXtalFreq - Center freq: %d, Tuner freq: %d\n",
		NewRTLFreq, NewTunerFreq)

	//---------- Get/Set Center Freq ----------
	err = u.dev.SetCenterFreq(CenterFreq)
	if err != nil {
		u.dev.Close()
		log.Printf("\tSetCenterFreq 978MHz Failed, error: %s\n", err)
		return
	}
	log.Printf("\tSetCenterFreq 978MHz Successful\n")

	log.Printf("\tGetCenterFreq: %d\n", u.dev.GetCenterFreq())

	//---------- Set Bandwidth ----------
	log.Printf("\tSetting Bandwidth: %d\n", Bandwidth)
	if err = u.dev.SetTunerBw(Bandwidth); err != nil {
		u.dev.Close()
		log.Printf("\tSetTunerBw %d Failed, error: %s\n", Bandwidth, err)
		return
	}
	log.Printf("\tSetTunerBw %d Successful\n", Bandwidth)

	if err = u.dev.ResetBuffer(); err != nil {
		u.dev.Close()
		log.Printf("\tResetBuffer Failed - error: %s\n", err)
		return
	}
	log.Printf("\tResetBuffer Successful\n")

	//---------- Get/Set Freq Correction ----------
	freqCorr := u.dev.GetFreqCorrection()
	log.Printf("\tGetFreqCorrection: %d\n", freqCorr)

	u.ppm = getPPM(u.serial)
	err = u.dev.SetFreqCorrection(u.ppm)
	if err != nil {
		u.dev.Close()
		log.Printf("\tSetFreqCorrection %d Failed, error: %s\n", u.ppm, err)
		return
	}
	hwHealth.RecordEstimatedPPM("uat", u.ppm)
	log.Printf("\tSetFreqCorrection %d Successful\n", u.ppm)

	return
}

// Read from the godump978 channel - on or off.
func uatReader() {
	log.Println("Entered uatReader() ...")
	for uat := range godump978.OutChan {
		TraceLog.Record(CONTEXT_GODUMP978, []byte(uat))
		handleUatMessage(uat)
	}
}

func handleUatMessage(uat string) {
	o, msgtype := parseInput(uat)
	if o != nil && msgtype != 0 {
		relayMessage(msgtype, o)
	}
}

func (u *UAT) writeID() error {
	info, err := u.dev.GetHwInfo()
	if err != nil {
		return err
	}
	info.Serial = "stratux:978"
	return u.dev.SetHwInfo(info)
}

func (e *ES) writeID() error {
	info, err := e.dev.GetHwInfo()
	if err != nil {
		return err
	}
	info.Serial = "stratux:1090"
	return e.dev.SetHwInfo(info)
}

func (f *OGN) writeID() error {
	info, err := f.dev.GetHwInfo()
	if err != nil {
		return err
	}
	info.Serial = "stratux:868"
	return f.dev.SetHwInfo(info)
}

func (f *AIS) writeID() error {
	info, err := f.dev.GetHwInfo()
	if err != nil {
		return err
	}
	info.Serial = "stratux:162"
	return f.dev.SetHwInfo(info)
}

func (u *UAT) shutdown() {
	log.Println("Entered UAT shutdown() ...")
	u.closeOnce.Do(func() {
		close(u.closeCh) // signal to shutdown
	})
	log.Println("UAT shutdown(): closing device to preempt ReadSync ...")
	if u.dev != nil {
		u.dev.Close() // preempt the blocking ReadSync call before waiting
	}
	log.Println("UAT shutdown(): calling u.wg.Wait() ...")
	u.wg.Wait() // Wait for the goroutine to shutdown
	log.Println("UAT shutdown() complete ...")
}

func (e *ES) shutdown() {
	log.Println("Entered ES shutdown() ...")
	e.closeOnce.Do(func() {
		close(e.closeCh) // signal to shutdown
	})
	log.Println("ES shutdown(): calling e.wg.Wait() ...")
	e.wg.Wait() // Wait for the goroutine to shutdown
	log.Println("ES shutdown() complete ...")
}

func (f *OGN) shutdown() {
	log.Println("Entered OGN shutdown() ...")
	f.closeOnce.Do(func() {
		close(f.closeCh) // signal to shutdown
	})
	log.Println("OGN shutdown(): calling f.wg.Wait() ...")
	f.wg.Wait() // Wait for the goroutine to shutdown
	log.Println("OGN shutdown() complete ...")
}

func (f *AIS) shutdown() {
	log.Println("Entered AIS shutdown() ...")
	f.closeOnce.Do(func() {
		close(f.closeCh) // signal to shutdown
	})
	log.Println("AIS shutdown(): calling f.wg.Wait() ...")
	f.wg.Wait() // Wait for the goroutine to shutdown
	log.Println("AIS shutdown() complete ...")
}

var (
	sdrShutdown       atomic.Bool
	sdrWatcherStarted atomic.Bool
	sdrDoneCh         = make(chan struct{})
)

func sdrKill() {
	// Send signal to shutdown to sdrWatcher().
	sdrShutdown.Store(true)
	if !sdrWatcherStarted.Load() {
		return
	}
	// Wait until sdrWatcher() has de-initialized all devices.
	<-sdrDoneCh
}

func reCompile(s string) *regexp.Regexp {
	// note , compile returns a nil pointer on error
	r, _ := regexp.Compile(s)
	return r
}

type regexUAT regexp.Regexp
type regexES regexp.Regexp
type regexOGN regexp.Regexp
type regexAIS regexp.Regexp

var rUAT = (*regexUAT)(reCompile("str?a?t?u?x:978"))
var rES = (*regexES)(reCompile("str?a?t?u?x:1090"))
var rOGN = (*regexOGN)(reCompile("str?a?t?u?x:868"))
var rAIS = (*regexAIS)(reCompile("str?a?t?u?x:162"))

func (r *regexUAT) hasID(serial string) bool {
	if r == nil {
		return strings.HasPrefix(serial, "stratux:978")
	}
	return (*regexp.Regexp)(r).MatchString(serial)
}

func (r *regexES) hasID(serial string) bool {
	if r == nil {
		return strings.HasPrefix(serial, "stratux:1090")
	}
	return (*regexp.Regexp)(r).MatchString(serial)
}

func (r *regexOGN) hasID(serial string) bool {
	if r == nil {
		return strings.HasPrefix(serial, "stratux:868")
	}
	return (*regexp.Regexp)(r).MatchString(serial)
}

func (r *regexAIS) hasID(serial string) bool {
	if r == nil {
		return strings.HasPrefix(serial, "stratux:162")
	}
	return (*regexp.Regexp)(r).MatchString(serial)
}

func createUATDev(id int, serial string, idSet bool) error {
	dev := &UAT{indexID: id, serial: serial}
	if err := dev.sdrConfig(); err != nil {
		log.Printf("UATDev.sdrConfig() failed: %s\n", err)
		return err
	}
	dev.wg = &sync.WaitGroup{}
	dev.idSet = idSet
	dev.closeCh = make(chan int)
	dev.wg.Add(1)
	sdrDevMutex.Lock()
	UATDev = dev
	sdrDevMutex.Unlock()
	go dev.read()
	return nil
}

func createESDev(id int, serial string, idSet bool) error {
	dev := &ES{indexID: id, serial: serial}
	if err := dev.sdrConfig(); err != nil {
		log.Printf("ESDev.sdrConfig() failed: %s\n", err)
		return err
	}
	dev.wg = &sync.WaitGroup{}
	dev.idSet = idSet
	dev.closeCh = make(chan int)
	dev.wg.Add(1)
	sdrDevMutex.Lock()
	ESDev = dev
	sdrDevMutex.Unlock()
	go dev.read()
	return nil
}

func createOGNDev(id int, serial string, idSet bool) error {
	dev := &OGN{indexID: id, serial: serial}
	if err := dev.sdrConfig(); err != nil {
		log.Printf("OGNDev.sdrConfig() failed: %s\n", err)
		return err
	}
	dev.wg = &sync.WaitGroup{}
	dev.idSet = idSet
	dev.closeCh = make(chan int)
	dev.wg.Add(1)
	sdrDevMutex.Lock()
	OGNDev = dev
	sdrDevMutex.Unlock()
	go dev.read()
	return nil
}

func createAISDev(id int, serial string, idSet bool) error {
	dev := &AIS{indexID: id, serial: serial}
	if err := dev.sdrConfig(); err != nil {
		log.Printf("AISDev.sdrConfig() failed: %s\n", err)
		return err
	}
	dev.wg = &sync.WaitGroup{}
	dev.idSet = idSet
	dev.closeCh = make(chan int)
	dev.wg.Add(1)
	sdrDevMutex.Lock()
	AISDev = dev
	sdrDevMutex.Unlock()
	go dev.read()
	return nil
}

func shutdownAllSDRDevices() {
	sdrDevMutex.Lock()
	uat, es, ogn, ais := UATDev, ESDev, OGNDev, AISDev
	UATDev, ESDev, OGNDev, AISDev = nil, nil, nil, nil
	sdrDevMutex.Unlock()

	if uat != nil {
		uat.shutdown()
	}
	if es != nil {
		es.shutdown()
	}
	if ogn != nil {
		ogn.shutdown()
	}
	if ais != nil {
		ais.shutdown()
	}
}

func configDevices(count int, esEnabled, uatEnabled, ognEnabled, aisEnabled bool) {
	// once the tagged dongles have been assigned, explicitly range over
	// the remaining IDs and assign them to any anonymous dongles
	unusedIDs := make(map[int]string)

	// loop 1: assign tagged dongles
	for i := 0; i < count; i++ {
		_, _, s, err := rtl.GetDeviceUsbStrings(i)
		if err == nil {
			//FIXME: Trim NULL from the serial. Best done in gortlsdr, but putting this here for now.
			s = strings.Trim(s, "\x00")
			// no need to check if createXDev returned an error; if it
			// failed to config the error is logged and we can ignore
			// it here so it doesn't get queued up again
			if uatEnabled && UATDev == nil && rUAT.hasID(s) {
				createUATDev(i, s, true)
			} else if esEnabled && ESDev == nil && rES.hasID(s) {
				createESDev(i, s, true)
			} else if ognEnabled && OGNDev == nil && rOGN.hasID(s) {
				createOGNDev(i, s, true)
			} else if aisEnabled && AISDev == nil && rAIS.hasID(s) {
				createAISDev(i, s, true)
			} else {
				unusedIDs[i] = s
			}
		} else {
			log.Printf("rtl.GetDeviceUsbStrings id %d: %s\n", i, err)
		}
	}

	// loop 2: assign anonymous dongles but sanity check the serial ids
	// so we don't cross config for dual assigned dongles or steal dongles
	// explicitly tagged for a disabled band.
	for i, s := range unusedIDs {
		if rUAT.hasID(s) || rES.hasID(s) || rOGN.hasID(s) || rAIS.hasID(s) {
			continue
		}
		if uatEnabled && !globalStatus.UATRadio_connected && UATDev == nil {
			createUATDev(i, s, false)
		} else if esEnabled && ESDev == nil {
			createESDev(i, s, false)
		} else if ognEnabled && OGNDev == nil {
			createOGNDev(i, s, false)
		} else if aisEnabled && AISDev == nil {
			createAISDev(i, s, false)
		}
	}
}

// to keep our sync primitives synchronized, only exit a read
// method's goroutine via the close flag channel check, to
// include catastrophic dongle failures
var (
	shutdownES  atomic.Bool
	shutdownUAT atomic.Bool
	shutdownOGN atomic.Bool
	shutdownAIS atomic.Bool
)

// Watch for config/device changes.
func sdrWatcher() {
	sdrWatcherStarted.Store(true)
	defer close(sdrDoneCh)

	prevCount := 0
	prevUATEnabled := false
	prevESEnabled := false
	prevOGNEnabled := false
	prevAISEnabled := false
	prevOGNTXEnabled := false
	globalSettingsMutex.RLock()
	prevdump1090Gain := globalSettings.Dump1090Gain
	devMode := globalSettings.DeveloperMode
	globalSettingsMutex.RUnlock()

	// Get the system (RPi) uptime.
	info := syscall.Sysinfo_t{}
	err := syscall.Sysinfo(&info)

	if err == nil && info.Uptime > 120 && devMode {
		// Throw a "critical error" if developer mode is enabled. Alerts the developer that the daemon was restarted (possibly)
		//  unexpectedly.
		addSingleSystemErrorf("restart-warn", "System uptime %d seconds. Daemon was restarted.\n", info.Uptime)
	}

	// Got system uptime. Delay SDR start for a bit to reduce noise for the GPS to get a fix.
	// Will give up waiting after 120s without fix
	for err == nil && info.Uptime < 120 && !isGPSValid() && !sdrShutdown.Load() {
		time.Sleep(1 * time.Second)
		err = syscall.Sysinfo(&info)
	}

	var lastHealthSample time.Time
	updateBandHealth := func(uatEn, esEn, ognEn, aisEn, pingEn, pongEn, uatRadioConn, pingConn, pongConn, ognConn, aisConn bool) {
		sdrDevMutex.RLock()
		hasUAT := UATDev != nil
		hasES := ESDev != nil
		hasOGN := OGNDev != nil
		hasAIS := AISDev != nil
		sdrDevMutex.RUnlock()

		hwHealth.SetBandStatus("uat", uatEn || pingEn || pongEn, hasUAT || uatRadioConn || pingConn || pongConn)
		hwHealth.SetBandStatus("es", esEn || pingEn || pongEn, hasES || pingConn || pongConn)
		hwHealth.SetBandStatus("ogn", ognEn, hasOGN || ognConn)
		hwHealth.SetBandStatus("ais", aisEn, hasAIS || aisConn)
	}

	for {
		time.Sleep(1 * time.Second)
		if sdrShutdown.Load() {
			shutdownAllSDRDevices()
			return
		}

		// true when a ReadSync call fails
		if shutdownUAT.Swap(false) {
			sdrDevMutex.Lock()
			dev := UATDev
			UATDev = nil
			sdrDevMutex.Unlock()
			if dev != nil {
				dev.shutdown()
			}
		}
		// true when subprocess startup fails
		if shutdownES.Swap(false) {
			sdrDevMutex.Lock()
			dev := ESDev
			ESDev = nil
			sdrDevMutex.Unlock()
			if dev != nil {
				dev.shutdown()
			}
		}
		if shutdownOGN.Swap(false) {
			sdrDevMutex.Lock()
			dev := OGNDev
			OGNDev = nil
			sdrDevMutex.Unlock()
			if dev != nil {
				dev.shutdown()
			}
		}
		if shutdownAIS.Swap(false) {
			sdrDevMutex.Lock()
			dev := AISDev
			AISDev = nil
			sdrDevMutex.Unlock()
			if dev != nil {
				dev.shutdown()
			}
		}

		// capture current state
		globalSettingsMutex.RLock()
		esEnabled := globalSettings.ES_Enabled
		uatEnabled := globalSettings.UAT_Enabled
		ognEnabled := globalSettings.OGN_Enabled
		aisEnabled := globalSettings.AIS_Enabled
		pingEnabled := globalSettings.Ping_Enabled
		pongEnabled := globalSettings.Pong_Enabled
		ognTXEnabled := globalSettings.OGNI2CTXEnabled
		dump1090Gain := globalSettings.Dump1090Gain
		autoGainEnabled := globalSettings.AutoGain_Enabled
		globalSettingsMutex.RUnlock()

		globalStatusMutex.RLock()
		uatRadioConnected := globalStatus.UATRadio_connected
		pingConnected := globalStatus.Ping_connected
		pongConnected := globalStatus.Pong_connected
		ognConnected := globalStatus.OGN_connected
		aisConnected := globalStatus.AIS_connected
		globalStatusMutex.RUnlock()

		// Every 5 seconds, sample UAT IQ clipping / dropped frames, update band status,
		// and apply UAT auto-gain step adjustments when enabled.
		if lastHealthSample.IsZero() || time.Since(lastHealthSample) >= 5*time.Second {
			lastHealthSample = time.Now()
			iq := godump978.GetIQStats(true)
			dropped := godump978.GetDroppedFrames()
			hwHealth.RecordUATIQStats(iq.ClippedPercent, iq.MeanAmplitude, dropped)

			if dump1090Gain > 0 {
				hwHealth.SetSuggestedESGain(dump1090Gain)
			}

			if autoGainEnabled && iq.TotalSamples > 0 {
				sdrDevMutex.Lock()
				if UATDev != nil && UATDev.dev != nil {
					currentUATGain := UATDev.gain
					if currentUATGain <= 0 {
						currentUATGain = float64(TunerGain) / 10.0
					}
					newGain, changed := ComputeGainStep(currentUATGain, iq.ClippedPercent, iq.MeanAmplitude < 6.0)
					if changed {
						gainTenths := int(math.Round(newGain * 10))
						if setErr := UATDev.dev.SetTunerGain(gainTenths); setErr != nil {
							log.Printf("UAT Auto-Gain: SetTunerGain(%.1f dB) failed: %s\n", newGain, setErr)
						} else {
							UATDev.gain = newGain
							log.Printf("UAT Auto-Gain: adjusted gain %.1f dB -> %.1f dB (clipped=%.2f%%, meanAmp=%.1f)\n",
								currentUATGain, newGain, iq.ClippedPercent, iq.MeanAmplitude)
						}
					}
				}
				sdrDevMutex.Unlock()
			}

			updateBandHealth(uatEnabled, esEnabled, ognEnabled, aisEnabled, pingEnabled, pongEnabled,
				uatRadioConnected, pingConnected, pongConnected, ognConnected, aisConnected)
		}

		count := rtl.GetDeviceCount()
		interfaceCount := count
		if uatRadioConnected {
			interfaceCount++
		}
		atomic.StoreUint32(&globalStatus.Devices, uint32(interfaceCount))

		// support up to 4 dongles (UAT, 1090ES, OGN, AIS)
		if count > 4 {
			count = 4
		}

		if interfaceCount == prevCount && prevESEnabled == esEnabled && prevUATEnabled == uatEnabled && prevOGNEnabled == ognEnabled && prevAISEnabled == aisEnabled &&
			prevOGNTXEnabled == ognTXEnabled && prevdump1090Gain == dump1090Gain {
			continue
		}

		// the device count or the global settings have changed, reconfig
		shutdownAllSDRDevices()
		configDevices(count, esEnabled, uatEnabled, ognEnabled, aisEnabled)
		if dump1090Gain > 0 {
			hwHealth.SetSuggestedESGain(dump1090Gain)
		}
		updateBandHealth(uatEnabled, esEnabled, ognEnabled, aisEnabled, pingEnabled, pongEnabled,
			uatRadioConnected, pingConnected, pongConnected, ognConnected, aisConnected)

		prevCount = interfaceCount
		prevUATEnabled = uatEnabled
		prevESEnabled = esEnabled
		prevOGNEnabled = ognEnabled
		prevAISEnabled = aisEnabled
		prevOGNTXEnabled = ognTXEnabled
		prevdump1090Gain = dump1090Gain

		countEnabled := 0
		if uatEnabled {
			countEnabled++
		}
		if esEnabled {
			countEnabled++
		}
		if ognEnabled {
			countEnabled++
		}
		if aisEnabled {
			countEnabled++
		}
		if countEnabled > interfaceCount {
			// User enabled too many protocols. Show error..
			used := make([]string, 0)
			sdrDevMutex.RLock()
			if UATDev != nil || uatRadioConnected {
				used = append(used, "UAT")
			}
			if ESDev != nil {
				used = append(used, "1090ES")
			}
			if OGNDev != nil {
				used = append(used, "OGN")
			}
			if AISDev != nil {
				used = append(used, "AIS")
			}
			sdrDevMutex.RUnlock()
			addSingleSystemErrorf("sdrconfig", "You have enabled more protocols than you have receivers for. "+
				"You have %d receivers, but enabled %d protocols. Please disable %d of them for things to work correctly. For now we are only using %s.",
				interfaceCount, countEnabled, countEnabled-interfaceCount, strings.Join(used, ", "))
		} else {
			removeSingleSystemError("sdrconfig")
		}
	}
}

func sdrInit() {
	go sdrWatcher()
	go uatReader()
	go godump978.ProcessDataFromChannel()
}
