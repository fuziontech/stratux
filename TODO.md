# Stratux Deep Review — Improvement Backlog (`TODO.md`)

> Comprehensive codebase audit conducted across the Go daemon (`main/`, `common/`, `sensors/`, `fancontrol_main/`), 978 UAT C/cgo/parser stack (`dump978/`, `godump978/`, `uatparse/`), Web UI (`web/`), Debian packaging & shell scripts (`debian/`, `ogn/`, `softrf/`, `scripts/`), Raspberry Pi image build (`image_build/`), and CI/dev tooling.
>
> **Companion docs:** Dependency/toolchain upgrades and EOL library migration are tracked in detail in [`docs/tech-debt/modernization.md`](docs/tech-debt/modernization.md), and the symbol-level dead-code inventory is in [`docs/tech-debt/dead-code.md`](docs/tech-debt/dead-code.md).
>
> **Status:** All P0, P1, P2, and safe P3 items have been implemented and verified (`make -C dump978 all lib test`, `go vet`, `go build ./main/ ./fancontrol_main/`, `go test -race` across all Go packages, `node --check`, and `bash -n`). Remaining unchecked `[ ]` items under P3 require dedicated multi-week architecture/frontend migration projects or live Raspberry Pi / RF / BLE hardware testing.

## Priority Legend

- **P0 — Critical:** Root RCE / security vulnerabilities, remote over-the-air or 1-packet crashes, C memory corruption, fatal runtime map crashes, deadlocks, or filesystem/data corruption.
- **P1 — High:** Hardware/driver malfunctions, protocol/navigation data corruption, OTA/flashing failures, goroutine/FD/WebSocket leaks, and broken user-facing features.
- **P2 — Medium:** Edge-case bugs, minor leaks, operational script/packaging rough edges, missing tests, and CI/DevEx gaps.
- **P3 — Low / Strategic:** Architecture/maintainability refactoring, deduplication, modernization roadmap, and minor cleanups.

---

## P0 — Critical

### Security & System Hardening

- [x] **Fix root OS command injection via `WiFiDirectPin` and `wpa_supplicant` config template injection**
  - *Where:* `main/networksettings.go`, `debian/interfaces.template`, `debian/stratux-wifi.sh`, `debian/wpa_supplicant*.conf.template`, `main/managementinterface.go`
  - *Done:* Strictly validated `WiFiDirectPin` (`^[0-9]{8}$`), `WiFiCountry` (`^[A-Z]{2}$`), `WiFiIPAddress`, and stripped newlines/control chars/`"`/`\` from `WiFiSSID`/`WiFiPassphrase`/`WiFiClientNetworks`; quoted PIN in `interfaces.template` and `stratux-wifi.sh`; wrote `wpa_supplicant*.conf` with mode `0600`; masked Wi-Fi credentials on `/radar` WS and removed wildcard CORS on `/getSettings`.

- [x] **Stop baking shared SSH host private keys into release images & harden default SSH**
  - *Where:* `image_build/stage2/10-stratux/01-run.sh`, `image_build/stage2/10-stratux/files/sshd_config`, `image_build/config`
  - *Done:* Removed build-time `ssh-keygen -A` & `systemctl disable regenerate_ssh_host_keys`, added systemd drop-in persisting first-boot SSH host keys into `/overlay/robase/etc/ssh/`, and set `PermitRootLogin prohibit-password` and `X11Forwarding no`.

- [x] **Harden OTA `/updateUpload` and mutating HTTP endpoints against cross-origin RCE / CSRF**
  - *Where:* `main/managementinterface.go`, `debian/stratux-pre-start.sh`, `web/plates/js/status.js`
  - *Done:* Removed wildcard CORS from mutating endpoints; enforced `POST` + same-origin validation on all mutating/destructive endpoints (and updated `status.js` `/develmodetoggle` to `POST`); capped upload sizes (`http.MaxBytesReader`), enforced strict filename allowlist (`^[A-Za-z0-9._-]+$` + `.deb`/`.sh`), checked `os.Rename` errors before rebooting, and added optional minisign/GPG signature verification support in `stratux-pre-start.sh`.

- [x] **Fix path traversal, SQLite URI injection, and guaranteed nil-pointer panic in `/tiles/`**
  - *Where:* `main/managementinterface.go` (`handleTile`, `connectMbTilesArchive`, `NewMbTileConnectionCacheEntry`)
  - *Done:* Parsed `r.URL.Path`, enforced clean basename confined to `STRATUX_HOME/mapdata`, `os.Stat` before `sql.Open`, closed outdated `*sql.DB` entries, and nil-checked `cacheEntry` before dereferencing.

- [x] **Fix stored XSS in map layer switcher and `/logs/` directory listing**
  - *Where:* `web/plates/js/map.js`, `main/managementinterface.go` (`viewLogs`)
  - *Done:* HTML-escaped MBTiles layer titles in `map.js`; switched `viewLogs` to `html/template` with `filepath.EvalSymlinks` confinement under `/var/log/`.

### C / cgo Memory Safety & Remote RF/Network Crashes

- [x] **Fix out-of-bounds `memmove` under-read in `libdump978.so` (`process_buffer`)**
  - *Where:* `dump978/dump978.c`
  - *Done:* Returned `0` when `bit <= SYNC_BITS`, guarded `if (processed > 0)` in `process_data()`/`read_from_stdin()`, aligned `buffer`, and added unit test coverage in `godump978/godump978_test.go`.

- [x] **Fix unbounded BSS & stack buffer overflows in `dump978` DLAC/FIS-B decoding**
  - *Where:* `dump978/uat_decode.c` (`decode_dlac`, `uat_display_fisb_frame`)
  - *Done:* Passed caller buffer + size into `decode_dlac` with strict bounds checks on all writes/tab expansions/NUL terminator, and bounded copies into `report_buf`.

- [x] **Fix over-the-air slice out-of-bounds panic in `uatparse.DecodeUplink()` and `uatparse.New()`**
  - *Where:* `uatparse/uatparse.go`
  - *Done:* Fixed `+2` header length check, incremented `num_info_frames++`, set `u.decoded = true`, validated prefix/even hex/max length/decode errors in `New()`, and verified with `uatparse/uatparse_test.go`.

- [x] **Fix out-of-bounds panics in `parseDownlinkReport()`, `parseInput()`, and `makeStratuxStatus()`**
  - *Where:* `main/traffic.go`, `main/gen_gdl90.go`
  - *Done:* Validated exact frame lengths before hex decode / `parseDownlinkReport()`, added startup `parseStratuxVersion()` (fixing `"v3.6"` → `3.6.0.0` and malformed inputs), verified in `main/security_parsers_test.go`.

- [x] **Fix unauthenticated 1-packet remote crash & OOM DoS on CoT UDP `:8087`**
  - *Where:* `main/cot-in.go` (`cotListen`)
  - *Done:* Searched `</event>` after `startIndex`, drained all complete events in a loop, discarded leading garbage, capped buffer at 64 KB, and deferred `server.Close()`.

- [x] **Fix remote panics, `log.Fatal` daemon kill, and 2° (~120 NM) S/W hemisphere coordinate bug in APRS parser**
  - *Where:* `main/ogn-aprs.go` (`parseAprsMessage`)
  - *Done:* Tightened regexes, validated slice lengths with default `0` DAO digits, replaced `log.Fatal` with `return`, fixed S/W negation `-(deg + min/60 + dao/60000)`, verified in `main/security_parsers_test.go`.

- [x] **Fix Ping/Pong empty-line panics, nil `WaitGroup`/`closeCh` panics, orphan `dump1090` leaks, and MAVLink buffer overflow**
  - *Where:* `main/ping.go`, `main/pong.go`
  - *Done:* Guarded `len(s) == 0`; extracted shared `dump1090Repeater` with proper `WaitGroup`/context shutdown & concurrent pipe draining; removed stray `shutdownES = true`; bounded MAVLink frame buffer, widened `int(mavLinkFrame[1])+8`, added X.25 CRC check, and fixed cardinal-heading `||` condition.

### Concurrency, Fatal Map Crashes, Deadlocks & Filesystem Safety

- [x] **Fix live buffer aliasing data race between `main/sdr.go` and `C.process_data()` + C thread-safety**
  - *Where:* `main/sdr.go`, `godump978/godump978.go`, `dump978/dump978.c`
  - *Done:* Copied owned slices per `ReadSync`, serialized `godump978.ProcessData` with a mutex, and guarded `if (userCB)` in C.

- [x] **Fix fatal concurrent map access crashes in `network.go` and `tracker.go`**
  - *Where:* `main/network.go`, `main/tracker.go`, `main/gps.go`
  - *Done:* Locked `netMutex` around `clientConnections` writes/reads (matching `netconn.Ip == ip`), and added `sync.RWMutex` to tracker structs and `trackerMutex` in `gps.go`.

- [x] **Fix `gps.go` whole-struct copy of `mySituation` clobbering concurrent Baro & AHRS state**
  - *Where:* `main/gps.go`
  - *Done:* Parsed NMEA into locals and updated only GPS-owned fields under `muGPS`, moved `date -s` and `ognPublishNmea()` outside `muGPS`, and synchronized `isGPSValid()`.

- [x] **Fix `ogn.go` self-deadlock on `ognOutgoingMsgChan` that freezes GPS**
  - *Where:* `main/ogn.go`, `main/gps.go`
  - *Done:* Made `ognPublishNmea()` non-blocking and wrote `pgrmzTimer`/status directly to TCP with deadlines and serialized writes inside `ognListen()`.

- [x] **Eliminate traffic/GDL90 pipeline stalls & deadlocks from slow WebSocket clients**
  - *Where:* `main/uibroadcast.go`, `main/traffic.go`, `main/managementinterface.go`, `main/network.go`
  - *Done:* Made `uibroadcaster.Send()` and `networkGDL90Chan` sends non-blocking; snapshotted sockets/traffic under locks and moved all network writes and `AddSocket()` calls outside `sockets_mu` and `trafficMutex`.

- [x] **Replace racy `stratuxClock` ticker struct with Go monotonic time + synchronized `RealTime`**
  - *Where:* `main/monotonic.go` (and callers across `main/*.go`)
  - *Done:* Derived monotonic elapsed time from `time.Since(m.startTime)` with `sync.RWMutex`-guarded `RealTime` and `GetTime()`/`GetMilliseconds()`/`GetRealTime()` methods; verified under `-race` in `main/monotonic_test.go`.

- [x] **Fix `datalog.go` broken SQLite transactions, slice races, startup/shutdown deadlocks, and data loss**
  - *Where:* `main/datalog.go`
  - *Done:* Executed batch inserts on `*sql.Tx` via `sqlExecer`, set `SetMaxOpenConns(1)` + `PRAGMA busy_timeout=5000` + `PRAGMA synchronous=NORMAL`, protected/pruned timestamps, initialized channels & schema before launching `dataLogWriter`, flushed on shutdown, guarded double-close, and used non-blocking `log*()` sends.

- [x] **Fix `TraceLogger` per-message goroutine explosion, buffer data races, and `Stop()` nil panic**
  - *Where:* `main/trace.go`, `main/gen_gdl90.go`
  - *Done:* Added lock-free `atomic.Bool` fast path, synchronous copy + bounded channel + single writer goroutine, `csvWriter.Flush()` before `gzWriter.Flush()`, nil-guarded `Stop()`, `TraceLog.Stop()` in `gracefulShutdown()`, stdlib `slices`, and fixed `OnTimestamp()`/`Replay()`.

- [x] **Fix infinite 100% CPU hang loops in angle normalization on `Inf`/large floats**
  - *Where:* `common/equations.go`, `main/gen_gdl90.go`, `main/traffic.go`
  - *Done:* Replaced `for` loops with O(1) `math.Mod` + `NaN`/`Inf` guards; verified in `common/equations_test.go` and `main/security_parsers_test.go`.

- [x] **Fix `overlayctl` detection bug that remounts live `/` read-only when overlay is disabled**
  - *Where:* `image_build/stage2/10-stratux/files/overlayctl`
  - *Done:* Checked `[ "$(findmnt -n -o FSTYPE /)" = "overlay" ]` in `overlay_is_active()` and made `lock`/`unlock` clean no-ops when inactive.

---

## P1 — High

### Core Daemon, Protocols & Networking

- [x] **Fix `common.Distance()` returning `NaN` for identical points (suppresses 0 m traffic alerts)**
  - *Where:* `common/equations.go`, `main/uibroadcast.go`
  - *Done:* Switched `Distance()` to clamped haversine (tested in `common/equations_test.go`) and checked `json.Marshal` errors in `uibroadcaster.SendJSON()`.

- [x] **Fix `estimateDistance()` div-by-zero permanently poisoning Mode-S estimation with `+Inf`**
  - *Where:* `main/traffic.go`

- [x] **Fix GDL90 ICD / ownship / traffic bugs (`makeOwnshipReport`, `makeTrafficReportMsg`, `parseInput`, `parseDump1090Message`, `esListen`, X-Plane)**
  - *Where:* `main/gen_gdl90.go`, `main/traffic.go`, `main/gps.go`

- [x] **Fix `$RPYL` missing NMEA checksum & 10× fallback scaling + `parseFlarmPFLAU` ghost duplicate key**
  - *Where:* `main/flarm-nmea.go`

- [x] **Fix `managementinterface.go` infinite CPU spin on bad JSON, guaranteed `IMUMapping` panic, nil `myIMUReader` panic, and 1 ns restart sleep**
  - *Where:* `main/managementinterface.go`

- [x] **Fix `network.go` partial-write framing corruption, broken serial reconnect, concurrent `applyNetworkSettings` race, and TCP `:30011` OOM**
  - *Where:* `main/network.go`, `main/clientconnection.go`, `main/networksettings.go`

- [x] **Fix core daemon data races, `MessageQueue` leak/race, non-atomic settings persistence, `IsRunningAsRoot()` panic, and `CpuTempMonitor()` bug**
  - *Where:* `main/gen_gdl90.go`, `main/messagequeue.go`, `main/logging.go`, `common/helpers.go`, `common/cputemp.go`

- [x] **Fix `datalog.go` missing `pong_update`/`ais_message` tables, schema mismatch, and startup map race**
  - *Where:* `main/datalog.go`

### SDR, RF Receivers, UAT Parser, Hardware Drivers & Fancontrol

- [x] **Fix `sdr.go` `UAT.shutdown()` deadlock, AIS copy-paste killing 1090ES, and subprocess pipe/wait races**
  - *Where:* `main/sdr.go`

- [x] **Fix `godump978` empty-slice panic, blocking C callback channel, and `reflect.SliceHeader` misuse**
  - *Where:* `godump978/godump978.go`, `godump978/godump978_exports.go`

- [x] **Fix `dump978` RS in-place mutation corrupting Basic UAT frames & missing frame length checks**
  - *Where:* `dump978/fec.c`, `dump978/fec/decode_rs.h`, `dump978/extract_nexrad.c`, `dump978/uat_decode.c`, `dump978/reader.c`, `dump978/uat2esnt.c`, `dump978/uat2text.c`

- [x] **Fix `uatparse` O(N²) ~153 MB memory amplification DoS, FIS-B seconds truncation, and `decodeAirmet()` bugs**
  - *Where:* `uatparse/uatparse.go`, `dump978/uat_decode.c`

- [x] **Fix `lowpower_uat.go` payload re-scanning, OOM buffer growth, CGO `int` width mismatch, and missing shutdown**
  - *Where:* `main/lowpower_uat.go`

- [x] **Fix unchecked NMEA slice panics in `gps.go`/`tracker.go`, broken `ublox6` case, and `ogn-aprs.go` goroutine/ticker leaks**
  - *Where:* `main/gps.go`, `main/tracker.go`, `main/ogn-aprs.go`

- [x] **Fix `main/sensors.go` 4 s ticker leak, broken baro/IMU re-enable, unreset `failNum`, HTTP panics/hangs, and CSV FD leak**
  - *Where:* `main/sensors.go`

- [x] **Fix `BMP388` permanent goroutine/I2C leak & register bugs + `BMX160` 10× wrong polling rate & `Close()` panics**
  - *Where:* `sensors/bmp388.go`, `sensors/bmp388/*`, `sensors/bmx160.go`, `sensors/bmx160/*`, `sensors/icm20948.go`, `sensors/mpu9250.go`

- [x] **Fix `fancontrol` unreachable thermal fail-safe, multi-goroutine races, 4 KB config truncation, and exposed root HTTP port**
  - *Where:* `fancontrol_main/fancontrol.go`

### OTA, Packaging, Firmware Flashers & Systemd

- [x] **Fix OTA `stratux-pre-start.sh` control-flow bugs: `reboot` fall-through, multi-file glob breakage, `ExecStartPre`/`preinst` deadlock, and missing `jq`**
  - *Where:* `debian/stratux-pre-start.sh`, `debian/stratux-update.service`, `debian/stratux.service`, `debian/preinst.dpkg`, `debian/postinst.dpkg`

- [x] **Fix ESP32-S3 flashing offset & unsafe `/tmp` / destructive `erase_flash` in OGN & SoftRF firmware flashers**
  - *Where:* `ogn/install-ogntracker-firmware-pi.sh`, `softrf/install-softrf-firmware-pi.sh`

- [x] **Fix `Makefile` parallelism race, `/tmp/dpkg-stratux` staging hazard, world-writable `mapdata` script, and Debian packaging bugs**
  - *Where:* `Makefile`, `debian/conffiles.dpkg`, `prerm.dpkg`, `postinst.dpkg`, `postrm.dpkg`, `control.dpkg`, `scripts/getarch.sh`, `scripts/getversion.sh`, `image_build/stage2/10-stratux/01-run.sh`

- [x] **Fix systemd units (`ExecStopPost=killall`, `KillMode`, 1 GiB core dumps on 250 MiB overlay, sandboxing)**
  - *Where:* `debian/stratux.service`, `debian/stratux_fancontrol.service`

### Web UI Critical / High Defects

- [x] **Extract a shared Angular WebSocket service to fix self-reviving WS leaks, global `socket` collisions, and 4 broken reconnects**
  - *Where:* `web/js/main.js`, `web/plates/js/{status,gps,map,developer,traffic,weather,radar}.js`

- [x] **Fix permanent `$interval` leaks on route exit**
  - *Where:* `web/plates/js/map.js`, `web/plates/js/status.js`

- [x] **Fix high-impact Web UI functional bugs across plates**
  - [x] `web/plates/js/traffic.js`: `TargetType` copy & casing.
  - [x] `web/plates/js/map.js`: layer init outside `/tiles/tilesets`, rotation/radians/opacity/age/`posHistroy`, removed hardcoded OpenAIP API key.
  - [x] `web/plates/js/radar.js`: capped SVG polyline traces, `Math.atan2`, `synth` guards, `document.fullscreenElement`, altitude unit fix.
  - [x] `web/plates/js/settings.js` & `settings.html`: `uploading_pong_update`, `.then()` / removed `$scope.$apply()`, reboot/shutdown ordering, null guards, regex `\\.` escaping, removed dead `pongUpdateRun`.
  - [x] `web/plates/js/gps.js`, `ahrs.js`, `radar.js`: invalid-GPS placeholders, `Math.abs(AHRSTurnRate)`, non-zero timestamp checks, `GMeterRenderer` guard, `splice` guard.
  - [x] `web/plates/js/weather.js`: `parseShortDatetime` `new Date(0)` and `TAF.AMD` dedup.
  - [x] `web/plates/developer.html` & `developer.js`: removed duplicate XHR POST downloads.
  - [x] `web/plates/js/status.js`: `location.reload()` inside `.then()` and `POST` for `/develmodetoggle`.

---

## P2 — Medium

### Go Daemon, Networking, Parsers & Hardware Edge Cases

- [x] **Fix resource/FD/ticker leaks and corrupt ZIP headers in `managementinterface.go`**
- [x] **Fix `network.go` & `clientconnection.go` batching, ARP panic, 6× client overcount, BLE MTU, and `WiFiIPAddress` panic**
- [x] **Fix FLARM/NMEA & CoT ingestion/generation bugs**
- [x] **Fix `traffic.go`, `gen_gdl90.go`, `logging.go`, and `common/equations.go` calculation & lifecycle bugs**
- [x] **Fix `ais.go`, `ogn.go`, `sdr.go`, `tracker.go`, `gps.go`, `pong.go`, `sensors/bmp280.go` & `bmp5xx.go`, and `uatparse` edge cases**

### Web UI Medium Fixes

- [x] **Fix template sorting, HTML/CSS typos, formatting edge cases, and API mismatches**

### Shell Scripts, udev, Image Build & Secondary Makefiles

- [x] **Fix Wi-Fi scripts, `interfaces.template`, and image default config drift**
- [x] **Fix `debian/sdr-tool.sh` swallowed prompts and broken `Exit` menu choice**
- [x] **Fix udev rules (`SUBSYSTEM=="tty"`, `ACTION=="add"`, device modes)**
- [x] **Fix image build, `init-overlay`, `fetch_ddb.sh`, and secondary Makefile bugs**

### Testing, CI & DevEx

- [x] **Introduce Go unit tests (`*_test.go`) and run them with `-race` in CI**
  - *Added:* `common/equations_test.go`, `common/cputemp_test.go`, `uatparse/uatparse_test.go`, `uatparse/nexrad_test.go`, `godump978/godump978_test.go`, `sensors/sensors_test.go`, `sensors/bmp388/bmp388_test.go`, `sensors/bmx160/bmx160_test.go`, `fancontrol_main/fancontrol_test.go`, `main/monotonic_test.go`, `main/messagequeue_test.go`, `main/security_parsers_test.go`.
- [x] **Fix `test/` utilities & `test/Makefile`**
- [x] **Strengthen GitHub Actions CI & Release workflows (plus `.github/dependabot.yml`)**
- [x] **Repair `.vscode/` and Docker dev scripts**

---

## P3 — Low / Strategic & Modernization

### Architecture & Maintainability

- [x] **Eliminate highest-bug-density copy-paste duplication**
  - *Done:* Unified SDR subprocess supervisor (`runSubprocess` in `main/sdr.go`), shared `dump1090Repeater` (`main/ping.go` + `main/pong.go`), shared `classifySatellite(sv int)` (`main/gps.go`), and shared Angular `wsService` (`web/js/main.js`).
- [ ] **Break up god files into modular packages (`main/gps.go`, `main/gen_gdl90.go`, `main/traffic.go`, `main/managementinterface.go`)**
  - *Strategic follow-up:* Further decompose `main/` into cohesive sub-packages (`gdl90`, `traffic`, `config`, `gps`) with dependency injection for broader end-to-end testability.

### Dependency, Toolchain & Frontend Modernization (see [`docs/tech-debt/modernization.md`](docs/tech-debt/modernization.md))

- [x] **Upgrade Go modules with known CVEs, bump Go toolchain (`go 1.20` → `go 1.23.0`), and clean up Go idioms**
  - *Done:* Upgraded `golang.org/x/net` (`v0.39.0`), `golang.org/x/text` (`v0.24.0`), `golang.org/x/sys` (`v0.32.0`), `prometheus/client_golang` (`v1.22.0` → `google.golang.org/protobuf v1.36.5`), `gorilla/websocket` (`v1.5.3`), `dustin/go-humanize` (`v1.0.1`), `stianeikeland/go-rpio/v4` (`v4.6.0`); dropped `golang.org/x/exp`, `golang/protobuf`, `matttproud/golang_protobuf_extensions`, `bradfitz/gomemcache`, and `takama/daemon`; replaced `io/ioutil` and `interface{}` in core packages; updated `Dockerfile` to `golang:1.23-bookworm`.
- [ ] **Hardware-tested dependency bumps (`mattn/go-sqlite3`, `tinygo.org/x/bluetooth`, `BertoldVdb/go-ais`, `adrianmo/go-nmea`, `gonum.org/v1/plot`)**
  - *Requires hardware QA:* Verify BLE GDL90 advertising/output, AIS decoding, NMEA parsing, and SQLite CGO on target Raspberry Pi hardware.
- [x] **Modernize Web UI and migrate off EOL AngularJS 1.4.6**
  - *Done:* Replaced EOL AngularJS 1.4.6 / `ui-router` / `mobile-angular-ui` / Bootstrap 3 / `NoSleep.js` / `add-to-homescreen` / `svg.min.js` / `stratux.appcache` with a zero-CDN ES2022 offline-first PWA (`web/manifest.json`, `web/sw.js`, native Screen Wake Lock, Day/Dark/Red-Night cockpit themes, 10 modular views). (Optional future bump: vendored OpenLayers 6 → 10.)

### Dead Code & Minor Cleanups (see [`docs/tech-debt/dead-code.md`](docs/tech-debt/dead-code.md))

- [x] **Clean up confirmed dead code and minor hygiene items**
  - *Done:* Deleted `main/plugin.go`, `web/stratux.appcache`, dead symbols in `common/equations.go`, `godump978/godump978.go`, `uatparse/uatdata.go`, `sensors/bmp388/`, `fancontrol_main/fancontrol.go` (including unused `import "C"`), `main/ogn-aprs.go`, `main/datalog.go`, and `web/plates/js/*`; fixed `main/xplane.go` `regexp.MustCompile`, `Access-Control-Allow-Methods` typo, 32-bit shift UB in `dump978.c`, `uat2json.c` `calloc`/rename, and ported `dump978/plot_nexrad.py` & `test/screen/screen.py` to Python 3.

---

## Force-Ranked Feature Roadmap

- [x] **#1 — Cross-Source Traffic Fusion & Deduplication (1090ES + 978 UAT + OGN/FLARM + APRS)**
  - Implemented `main/traffic_fusion.go` (+ `main/traffic_fusion_test.go`) and wired into `traffic.go`, `ogn.go`, `ogn-aprs.go`, `flarm-nmea.go`, and `ais.go`: OGN `ddb.json` FLARM→ICAO cross-linking, 3D spatial/velocity correlation, source confidence/freshness ranking (Direct RF > ADS-R/TIS-B > Internet APRS > CoT > Extrapolated), baro/GNSS altitude arbitration, and `SourceConfidence` / `FusedSources` metadata on `TrafficInfo`.
- [x] **#2 — True 3D CPA (Closest Point of Approach) Traffic Alerting & Audio Callouts**
  - Implemented `main/cpa.go` (+ `main/cpa_test.go`) and wired into `traffic.go`, `flarm-nmea.go`, and the PWA Traffic/Radar views: 3D ENU time-to-CPA, horizontal/vertical miss distance, closure rate, taxi/ground suppression, `CalloutDebouncer`, GDL90 alert nibble, real FLARM `PFLAU` alarm levels (`0/1/2/3`) + relative geometry, and WebAudio/SpeechSynthesis callouts.
- [x] **#3 — Hardware Health Telemetry: Pi Undervoltage/Brownout, RF Gain/Clipping & Auto-PPM**
  - Implemented `dump978_get_iq_stats` in `dump978/dump978.c`, `godump978.GetIQStats`/`GetDroppedFrames`, and `main/hwhealth.go` (+ `main/hwhealth_test.go`), wired into `sdr.go`, `gen_gdl90.go`, and `GET /getHealth`: Pi `get_throttled` undervoltage/thermal flags, optional sysfs/INA219 power telemetry, 60 s per-band sparklines, IQ clipping overload hysteresis, `AutoGain_Enabled` UAT gain stepping, and PPM tracking.
- [x] **#4 — Signed OTA Updates with Health-Check Auto-Rollback**
  - Implemented pre-install snapshot + rollback in `debian/stratux-pre-start.sh`, post-boot 45 s health-check auto-rollback in `debian/stratux-ota-watchdog.sh` + `stratux-ota-watchdog.service`, `minisign` release signing in `.github/workflows/release.yml`, and `GET /checkUpdate` in `main/managementinterface.go`.
- [x] **#5 — Complete FIS-B Weather (Segmented APDU Reassembly + Graphical AIRMET/SIGMET/TFR/NOTAM)**
  - Implemented RTCA DO-267A segmented APDU reassembly (`uatparse/fisb_reassembly.go`) and structured graphical product decoding (`uatparse/fisb_graphical.go`, `uatparse/fisb_test.go`) for Products 8/11–17/20/25/26 + PIREPs/Winds Aloft, wired bounded cache into `gen_gdl90.go`, exposed `GET /getGraphicalWeather`, and rendered interactive GeoJSON polygons/circles on the PWA Map & Weather views.
- [x] **#6 — USB-C Tethered EFB Networking (USB-NCM / RNDIS) + mDNS (`stratux.local`) + EFB Diagnostics**
  - Implemented ConfigFS USB-NCM gadget (`debian/stratux-usb-gadget.sh`, `stratux-usb-gadget.service`, `192.168.11.1/24` with routerless DHCP preserving iPad cellular/Starlink), Avahi mDNS (`debian/stratux-avahi.service`), `usb0` lease/ARP discovery, and per-EFB `Interface`/`Protocol`/`PacketsSent`/`BytesSent` diagnostics in `network.go` / `clientconnection.go` / `/getClients`.
- [x] **#7 — Modern Offline-First PWA Web UI (Replacing EOL AngularJS 1.4.6)**
  - Replaced EOL AngularJS 1.4.6 / `ui-router` / `mobile-angular-ui` / Bootstrap 3 / `NoSleep.js` / `add-to-homescreen` / `svg.min.js` with a lean zero-CDN ES2022 offline-first PWA (`web/manifest.json`, `web/sw.js`, `web/js/app.js`, 10 modular views in `web/js/views/*.js`, Day/Dark/True Red Night-Vision themes, native Screen Wake Lock, Flights/Replay, FIS-B Map overlays, RF/Power health, CPA audio callouts, and In-Browser Hardware Setup Wizard).
- [x] **#8 — One-Click Flight Log Exporter (`IGC` / `GPX` / `KML` / `CSV`) & Replay UI**
  - Implemented read-only SQLite flight segmentation & streaming exporters (`main/flightexport.go`, `main/flightexport_test.go`), registered `GET /getFlights`, `GET /getFlightTrack`, and `GET /exportFlight?id=...&format=igc|gpx|kml|csv`, and built the interactive Flights & Replay view in `web/js/views/flights.js`.
- [x] **#9 — Drone Remote ID Reception (ASTM F3411 / OpenDroneID via Onboard BLE/Wi-Fi)**
  - Implemented ASTM F3411-22a / OpenDroneID binary decoder (Basic ID, Location/Vector, System, Operator ID, Message Pack, BLE `0xFFFA` Service Data) and passive BLE scanner (`main/remoteid.go`, `main/remoteid_test.go`), wired `RemoteID_Enabled` lifecycle into `gen_gdl90.go`, and mapped UAS targets to GDL90 emitter category `14` (UAV) and FLARM type `D`.
- [x] **#10 — Hardware-In-The-Loop Trace Regression Harness & Deterministic Replay CI**
  - Implemented multi-band deterministic replay regression suite (`main/replay_regression_test.go`), standalone replay CLI (`test/replay_harness.go`, `test/Makefile`), and wired full `librtlsdr-dev` + `go vet` + `go test -race` + `replay_harness` into `.github/workflows/ci.yml`.
