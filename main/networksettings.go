/*
	Copyright (c) 2019 Adrian Batzill
	Distributable under the terms of The "BSD New" License
	that can be found in the LICENSE file, herein included
	as part of this header.

	networksettings.go: Management functions for network settings (wpa_supplicant, IP, DHCP)
*/

package main

import (
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"
)

const (
	WifiModeAp       = 0
	WifiModeDirect   = 1
	WifiModeApClient = 2

	defaultWiFiIPAddress = "192.168.10.1"
	defaultWiFiCountry   = "US"
	defaultWiFiDirectPin = "12345678"
	defaultWiFiSSID      = "Stratux"
)

// NetworkTemplateParams is passed to the template engine to write settings
type NetworkTemplateParams struct {
	WiFiMode                       int
	WiFiCountry                    string
	IpAddr                         string
	IpPrefix                       string
	DhcpRangeStart                 string
	DhcpRangeEnd                   string
	WiFiSSID                       string
	WiFiChannel                    int
	WiFiDirectPin                  string
	WiFiPassPhrase                 string
	WiFiClientNetworks             []wifiClientNetwork
	WiFiInternetPassThroughEnabled bool
}

type wifiClientNetwork struct {
	SSID     string
	Password string
}

var (
	wifiDirectPinRegex = regexp.MustCompile(`^[0-9]{8}$`)
	wifiCountryRegex   = regexp.MustCompile(`^[A-Z]{2}$`)

	hasChanged        bool
	hasChangedMutex   sync.Mutex
	applyNetworkMutex sync.Mutex
)

// isSafeWpaString returns true if s contains no characters that could inject
// directives or break quoting in wpa_supplicant*.conf templates (newlines, NUL,
// ASCII control characters, double quotes, or backslashes).
func isSafeWpaString(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == 0x7f || c == '"' || c == '\\' {
			return false
		}
	}
	return true
}

// sanitizeWpaString strips any unsafe characters as defense-in-depth when
// rendering templates from values loaded out of stratux.conf.
func sanitizeWpaString(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == 0x7f || c == '"' || c == '\\' {
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func validateIPv4(ip string) (string, bool) {
	parsed := net.ParseIP(strings.TrimSpace(ip))
	if parsed == nil {
		return "", false
	}
	v4 := parsed.To4()
	if v4 == nil {
		return "", false
	}
	return v4.String(), true
}

func setWifiCountry(countryCode string) {
	countryCode = strings.ToUpper(strings.TrimSpace(countryCode))
	if !wifiCountryRegex.MatchString(countryCode) {
		log.Printf("Ignoring invalid WiFiCountry %q; using fallback %s", countryCode, defaultWiFiCountry)
		countryCode = defaultWiFiCountry
	}
	globalSettingsMutex.Lock()
	changed := countryCode != globalSettings.WiFiCountry
	if changed {
		globalSettings.WiFiCountry = countryCode
	}
	globalSettingsMutex.Unlock()

	if changed {
		hasChangedMutex.Lock()
		hasChanged = true
		hasChangedMutex.Unlock()
	}
}

func setWifiSSID(ssid string) {
	if !isSafeWpaString(ssid) || len(ssid) > 32 {
		log.Printf("Ignoring invalid WiFiSSID")
		return
	}
	globalSettingsMutex.Lock()
	changed := ssid != globalSettings.WiFiSSID
	if changed {
		globalSettings.WiFiSSID = ssid
	}
	globalSettingsMutex.Unlock()

	if changed {
		hasChangedMutex.Lock()
		hasChanged = true
		hasChangedMutex.Unlock()
	}
}

func setWifiPassphrase(passphrase string) {
	if !isSafeWpaString(passphrase) || len(passphrase) > 64 {
		log.Printf("Ignoring invalid WiFiPassphrase")
		return
	}
	globalSettingsMutex.Lock()
	changed := passphrase != globalSettings.WiFiPassphrase
	if changed {
		globalSettings.WiFiPassphrase = passphrase
	}
	globalSettingsMutex.Unlock()

	if changed {
		hasChangedMutex.Lock()
		hasChanged = true
		hasChangedMutex.Unlock()
	}
}

func setWifiChannel(channel int) {
	if channel < 0 || channel > 14 {
		log.Printf("Ignoring invalid WiFiChannel: %d", channel)
		return
	}
	globalSettingsMutex.Lock()
	changed := channel != globalSettings.WiFiChannel
	if changed {
		globalSettings.WiFiChannel = channel
	}
	globalSettingsMutex.Unlock()

	if changed {
		hasChangedMutex.Lock()
		hasChanged = true
		hasChangedMutex.Unlock()
	}
}

func setWifiSecurityEnabled(enabled bool) {
	globalSettingsMutex.Lock()
	changed := globalSettings.WiFiSecurityEnabled != enabled
	if changed {
		globalSettings.WiFiSecurityEnabled = enabled
	}
	globalSettingsMutex.Unlock()

	if changed {
		hasChangedMutex.Lock()
		hasChanged = true
		hasChangedMutex.Unlock()
	}
}

func setWifiIPAddress(ip string) {
	cleanIP, ok := validateIPv4(ip)
	if !ok {
		log.Printf("Ignoring invalid IP Address: %s", ip)
		return
	}
	globalSettingsMutex.Lock()
	changed := globalSettings.WiFiIPAddress != cleanIP
	if changed {
		globalSettings.WiFiIPAddress = cleanIP
	}
	globalSettingsMutex.Unlock()

	if changed {
		hasChangedMutex.Lock()
		hasChanged = true
		hasChangedMutex.Unlock()
	}
}

func setWiFiMode(mode int) {
	if mode != WifiModeAp && mode != WifiModeDirect && mode != WifiModeApClient {
		log.Printf("Ignoring invalid WiFiMode: %d", mode)
		return
	}
	globalSettingsMutex.Lock()
	changed := globalSettings.WiFiMode != mode
	if changed {
		globalSettings.WiFiMode = mode
	}
	globalSettingsMutex.Unlock()

	if changed {
		hasChangedMutex.Lock()
		hasChanged = true
		hasChangedMutex.Unlock()
	}
}

func setWifiDirectPin(pin string) {
	if !wifiDirectPinRegex.MatchString(pin) {
		log.Printf("Ignoring invalid WiFiDirectPin (must be exactly 8 digits)")
		return
	}
	globalSettingsMutex.Lock()
	changed := globalSettings.WiFiDirectPin != pin
	if changed {
		globalSettings.WiFiDirectPin = pin
	}
	globalSettingsMutex.Unlock()

	if changed {
		hasChangedMutex.Lock()
		hasChanged = true
		hasChangedMutex.Unlock()
	}
}

func setWifiClientNetworks(networks []wifiClientNetwork) {
	sanitized := make([]wifiClientNetwork, 0, len(networks))
	for _, n := range networks {
		if n.SSID == "" || len(n.SSID) > 32 || !isSafeWpaString(n.SSID) || len(n.Password) > 64 || !isSafeWpaString(n.Password) {
			log.Printf("Ignoring invalid WiFiClientNetwork entry")
			continue
		}
		sanitized = append(sanitized, n)
	}

	globalSettingsMutex.Lock()
	changed := len(globalSettings.WiFiClientNetworks) != len(sanitized)
	if !changed {
		for i, n := range sanitized {
			if globalSettings.WiFiClientNetworks[i].SSID != n.SSID || globalSettings.WiFiClientNetworks[i].Password != n.Password {
				changed = true
				break
			}
		}
	}
	if changed {
		globalSettings.WiFiClientNetworks = sanitized
	}
	globalSettingsMutex.Unlock()

	if changed {
		hasChangedMutex.Lock()
		hasChanged = true
		hasChangedMutex.Unlock()
	}
}

func setWifiInternetPassthroughEnabled(enabled bool) {
	globalSettingsMutex.Lock()
	changed := globalSettings.WiFiInternetPassThroughEnabled != enabled
	if changed {
		globalSettings.WiFiInternetPassThroughEnabled = enabled
	}
	globalSettingsMutex.Unlock()

	if changed {
		hasChangedMutex.Lock()
		hasChanged = true
		hasChangedMutex.Unlock()
	}
}

// if onlyWriteFiles is true, we only write the config files. Otherwise we also reconfigure the network.
// Also, if we only write the files, this function runs synchronously. Otherwise the long-running network reconfiguration is done async.
func applyNetworkSettings(force bool, onlyWriteFiles bool) {
	hasChangedMutex.Lock()
	if !hasChanged && !force {
		hasChangedMutex.Unlock()
		return
	}
	hasChanged = false
	hasChangedMutex.Unlock()

	// Snapshot settings safely under globalSettingsMutex.
	globalSettingsMutex.RLock()
	ipAddr := globalSettings.WiFiIPAddress
	wifiMode := globalSettings.WiFiMode
	wifiChannel := globalSettings.WiFiChannel
	wifiCountry := globalSettings.WiFiCountry
	wifiSSID := globalSettings.WiFiSSID
	wifiDirectPin := globalSettings.WiFiDirectPin
	wifiSecurityEnabled := globalSettings.WiFiSecurityEnabled
	wifiPassphrase := globalSettings.WiFiPassphrase
	wifiInternetPassthrough := globalSettings.WiFiInternetPassThroughEnabled
	rawClientNetworks := make([]wifiClientNetwork, len(globalSettings.WiFiClientNetworks))
	copy(rawClientNetworks, globalSettings.WiFiClientNetworks)
	globalSettingsMutex.RUnlock()

	// Validate IPv4 address strictly before splitting octets.
	cleanIP, ok := validateIPv4(ipAddr)
	if !ok {
		if ipAddr != "" {
			log.Printf("Invalid WiFiIPAddress %q, falling back to %s", ipAddr, defaultWiFiIPAddress)
		}
		cleanIP = defaultWiFiIPAddress
	}
	ipAddr = cleanIP
	log.Printf("Applying new network settings for IP %s", ipAddr)

	ipParts := strings.Split(ipAddr, ".")
	ipPrefix := ipParts[0] + "." + ipParts[1] + "." + ipParts[2]

	myIP, _ := strconv.Atoi(ipParts[3])
	dhcpRangeStart := ipPrefix + ".10"
	dhcpRangeEnd := ipPrefix + ".50"
	if myIP >= 10 && myIP <= 50 {
		// In case the stratux ip is inside its dhcp range, we move the dhcp range back to something else..
		dhcpRangeStart = ipPrefix + ".60"
		dhcpRangeEnd = ipPrefix + ".110"
	}

	// Re-validate / sanitize all fields before template rendering (defense in depth against tampered stratux.conf).
	wifiCountry = strings.ToUpper(strings.TrimSpace(wifiCountry))
	if !wifiCountryRegex.MatchString(wifiCountry) {
		wifiCountry = defaultWiFiCountry
	}
	if !wifiDirectPinRegex.MatchString(wifiDirectPin) {
		if wifiDirectPin != "" {
			log.Printf("Invalid WiFiDirectPin in settings, falling back to default")
		}
		wifiDirectPin = defaultWiFiDirectPin
	}
	wifiSSID = sanitizeWpaString(wifiSSID)
	if wifiSSID == "" {
		wifiSSID = defaultWiFiSSID
	}
	if wifiChannel <= 0 || wifiChannel > 14 {
		wifiChannel = 1
	}

	clientNetworks := make([]wifiClientNetwork, 0, len(rawClientNetworks))
	for _, n := range rawClientNetworks {
		cleanSSID := sanitizeWpaString(n.SSID)
		cleanPass := sanitizeWpaString(n.Password)
		if cleanSSID == "" {
			continue
		}
		clientNetworks = append(clientNetworks, wifiClientNetwork{
			SSID:     cleanSSID,
			Password: cleanPass,
		})
	}

	var tplSettings NetworkTemplateParams
	tplSettings.WiFiMode = wifiMode
	tplSettings.IpAddr = ipAddr
	tplSettings.IpPrefix = ipPrefix
	tplSettings.DhcpRangeStart = dhcpRangeStart
	tplSettings.DhcpRangeEnd = dhcpRangeEnd
	tplSettings.WiFiChannel = wifiChannel
	tplSettings.WiFiCountry = wifiCountry
	tplSettings.WiFiSSID = wifiSSID
	tplSettings.WiFiDirectPin = wifiDirectPin
	tplSettings.WiFiClientNetworks = clientNetworks
	tplSettings.WiFiInternetPassThroughEnabled = wifiInternetPassthrough

	if wifiSecurityEnabled || tplSettings.WiFiMode == WifiModeDirect {
		tplSettings.WiFiPassPhrase = sanitizeWpaString(wifiPassphrase)
	}

	f := func() {
		applyNetworkMutex.Lock()
		defer applyNetworkMutex.Unlock()

		if !onlyWriteFiles {
			time.Sleep(time.Second)
			cmd := exec.Command("ifdown", "wlan0")
			if err := cmd.Run(); err != nil {
				log.Printf("Error shutting down WiFi: %s\n", err.Error())
			}
		}

		overlayctl("unlock")
		writeTemplate(STRATUX_HOME+"/cfg/stratux-dnsmasq.conf.template", "/overlay/robase/etc/dnsmasq.d/stratux-dnsmasq.conf", tplSettings)
		writeTemplate(STRATUX_HOME+"/cfg/interfaces.template", "/overlay/robase/etc/network/interfaces", tplSettings)
		writeTemplate(STRATUX_HOME+"/cfg/wpa_supplicant.conf.template", "/overlay/robase/etc/wpa_supplicant/wpa_supplicant.conf", tplSettings)
		writeTemplate(STRATUX_HOME+"/cfg/wpa_supplicant_ap.conf.template", "/overlay/robase/etc/wpa_supplicant/wpa_supplicant_ap.conf", tplSettings)
		overlayctl("lock")

		if !onlyWriteFiles {
			cmd := exec.Command("ifup", "wlan0")
			if err := cmd.Run(); err != nil {
				log.Printf("Error starting WiFi: %s\n", err.Error())
			}
		}
	}

	if onlyWriteFiles {
		f()
	} else {
		go f()
	}
}

func writeTemplate(tplFile string, outFile string, settings NetworkTemplateParams) {
	configTemplate, err := template.ParseFiles(tplFile)
	if err != nil {
		log.Printf("Network Settings: Unable to read settings template %s: %s", tplFile, err)
		return
	}

	mode := os.FileMode(0644)
	if strings.HasPrefix(filepath.Base(outFile), "wpa_supplicant") {
		mode = 0600
	}

	outputFile, err := os.OpenFile(outFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		log.Printf("Network Settings: Unable to open output file %s: %s", outFile, err)
		return
	}

	if err := outputFile.Chmod(mode); err != nil {
		log.Printf("Network Settings: Unable to set permissions on %s: %s", outFile, err)
	}

	if err := configTemplate.Execute(outputFile, settings); err != nil {
		log.Printf("Network Settings: Unable to execute template substitution %s: %s", outFile, err)
		_ = outputFile.Close()
		return
	}
	if err := outputFile.Sync(); err != nil {
		log.Printf("Network Settings: Unable to sync output file %s: %s", outFile, err)
	}
	if err := outputFile.Close(); err != nil {
		log.Printf("Network Settings: Unable to close output file %s: %s", outFile, err)
	}
}
