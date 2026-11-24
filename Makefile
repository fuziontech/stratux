export STRATUX_HOME := /opt/stratux
export DEBPKG_BASE := $(CURDIR)/build/dpkg/stratux
export DEBPKG_HOME := $(DEBPKG_BASE)/opt/stratux
VERSIONSTR := $(shell ./scripts/getversion.sh)
ARCH := $(shell ./scripts/getarch.sh)
BUILDHASH := $(shell git log -n 1 --pretty=%H 2>/dev/null || echo unknown)

LFLAGS=-X main.stratuxVersion=$(VERSIONSTR) -X main.stratuxBuild=$(BUILDHASH)
BUILDINFO=-ldflags "$(LFLAGS)"
BUILDINFO_STATIC=-ldflags "-extldflags -static $(LFLAGS)"
PLATFORMDEPENDENT=fancontrol

ifeq ($(debug),true)
	BUILDINFO := -gcflags '-N -l' $(BUILDINFO)
endif

ifeq ($(ARCH),arm64)
	OGN_RX_BINARY=ogn/ogn-rx-eu_aarch64
else ifeq ($(ARCH),amd64)
	OGN_RX_BINARY=ogn/ogn-rx-eu_x86
else
	OGN_RX_BINARY=ogn/ogn-rx-eu_arm
endif

STRATUX_GO_DEPS := $(wildcard main/*.go common/*.go config/*.go gdl90/*.go gpsnmea/*.go godump978/*.go uatparse/*.go sensors/*.go sensors/*/*.go) go.mod go.sum
DUMP978_DEPS := $(wildcard dump978/*.c dump978/*.h dump978/fec/*.c dump978/fec/*.h)

.PHONY: all clean install optinstall _optinstall_files www dpkg prep_dpkg optinstall_dpkg dall ddpkg xdump1090 xrtlais test

all: libdump978.so xdump1090 xrtlais stratuxrun $(PLATFORMDEPENDENT)

stratuxrun: $(STRATUX_GO_DEPS) libdump978.so
	LIBRARY_PATH=$(CURDIR) CGO_CFLAGS_ALLOW="-L$(CURDIR)" go build $(BUILDINFO) -o stratuxrun ./main/

fancontrol: $(wildcard fancontrol_main/*.go common/*.go) go.mod go.sum
	go build $(BUILDINFO) -o fancontrol -p 4 ./fancontrol_main/

xdump1090:
	make -C dump1090 BLADERF=no

libdump978.so: $(DUMP978_DEPS)
	$(MAKE) -C dump978 lib

xrtlais:
	make -C rtl-ais LDFLAGS="-lpthread -lm -lrtlsdr"

test:
	$(MAKE) -C test

www:
	$(MAKE) -C web STRATUX_HOME=$(STRATUX_HOME)

ogn/ddb.json:
	cd ogn && ./fetch_ddb.sh

optinstall: all ogn/ddb.json
	$(MAKE) STRATUX_HOME=$(STRATUX_HOME) _optinstall_files

_optinstall_files:
	$(MAKE) -C web STRATUX_HOME=$(STRATUX_HOME)
	mkdir -p $(STRATUX_HOME)/bin
	mkdir -p $(STRATUX_HOME)/www
	mkdir -p $(STRATUX_HOME)/ogn
	mkdir -p $(STRATUX_HOME)/softrf
	mkdir -p $(STRATUX_HOME)/cfg
	mkdir -p $(STRATUX_HOME)/lib
	mkdir -p $(STRATUX_HOME)/mapdata
	chmod 0775 $(STRATUX_HOME)/mapdata

	# binaries
	cp -f stratuxrun $(STRATUX_HOME)/bin/
	cp -f fancontrol $(STRATUX_HOME)/bin/
	cp -f dump1090/dump1090 $(STRATUX_HOME)/bin/
	cp -f rtl-ais/rtl_ais $(STRATUX_HOME)/bin/
	cp -f $(OGN_RX_BINARY) $(STRATUX_HOME)/bin/ogn-rx-eu

	# Libs
	cp -f libdump978.so $(STRATUX_HOME)/lib/

	# map data (keep executable helper script out of user-writable mapdata dir)
	find mapdata -maxdepth 1 -type f ! -name 'download_mapdata.sh' -exec cp -f {} $(STRATUX_HOME)/mapdata/ \;

	# OGN stuff
	cp -f ogn/ddb.json ogn/*ogn-tracker-bin-*.zip ogn/install-ogntracker-firmware-pi.sh ogn/fetch_ddb.sh $(STRATUX_HOME)/ogn/
	cp -f softrf/*.zip softrf/*.sh $(STRATUX_HOME)/softrf/

	# Scripts
	cp -f debian/stratux-pre-start.sh $(STRATUX_HOME)/bin/stratux-pre-start.sh
	cp -f debian/stratux-ota-watchdog.sh $(STRATUX_HOME)/bin/stratux-ota-watchdog.sh
	cp -f debian/stratux-usb-gadget.sh $(STRATUX_HOME)/bin/stratux-usb-gadget.sh
	cp -f debian/stratux-wifi.sh $(STRATUX_HOME)/bin/
	cp -f debian/sdr-tool.sh $(STRATUX_HOME)/bin/
	cp -f mapdata/download_mapdata.sh $(STRATUX_HOME)/bin/download_mapdata.sh
	chmod 0755 $(STRATUX_HOME)/bin/*

	# Config templates
	cp -f debian/stratux-dnsmasq.conf.template $(STRATUX_HOME)/cfg/
	cp -f debian/interfaces.template $(STRATUX_HOME)/cfg/
	cp -f debian/wpa_supplicant.conf.template $(STRATUX_HOME)/cfg/
	cp -f debian/wpa_supplicant_ap.conf.template $(STRATUX_HOME)/cfg/
	chmod 0644 $(STRATUX_HOME)/cfg/*.template


install: dpkg
	dpkg -i stratux-$(VERSIONSTR)-$(ARCH).deb

#
# Debian package related targets below
#

prep_dpkg:
	rm -rf $(DEBPKG_BASE)
	mkdir -p $(DEBPKG_BASE)/DEBIAN

optinstall_dpkg: prep_dpkg all ogn/ddb.json
	$(MAKE) STRATUX_HOME=$(DEBPKG_HOME) _optinstall_files

dpkg: optinstall_dpkg
	# Copy the control, conffiles, and maintainer scripts to DEBIAN directory
	cp -f debian/control.dpkg $(DEBPKG_BASE)/DEBIAN/control
	cp -f debian/conffiles.dpkg $(DEBPKG_BASE)/DEBIAN/conffiles
	cp -f debian/preinst.dpkg $(DEBPKG_BASE)/DEBIAN/preinst
	cp -f debian/postinst.dpkg $(DEBPKG_BASE)/DEBIAN/postinst
	cp -f debian/prerm.dpkg $(DEBPKG_BASE)/DEBIAN/prerm
	cp -f debian/postrm.dpkg $(DEBPKG_BASE)/DEBIAN/postrm
	# Create the directories inside of the dpkg environment
	mkdir -p $(DEBPKG_BASE)/etc/udev/rules.d/
	mkdir -p $(DEBPKG_BASE)/etc/avahi/services/
	mkdir -p $(DEBPKG_BASE)/usr/lib/systemd/system/
	# Copy the udev rules to the dpkg environment
	cp -f debian/10-stratux.rules $(DEBPKG_BASE)/etc/udev/rules.d/10-stratux.rules
	cp -f debian/99-uavionix.rules $(DEBPKG_BASE)/etc/udev/rules.d/99-uavionix.rules
	cp -f debian/99-pong.rules $(DEBPKG_BASE)/etc/udev/rules.d/99-pong.rules
	chmod 0644 $(DEBPKG_BASE)/etc/udev/rules.d/*.rules
	# Copy the Avahi mDNS service definition
	cp -f debian/stratux-avahi.service $(DEBPKG_BASE)/etc/avahi/services/stratux.service
	chmod 0644 $(DEBPKG_BASE)/etc/avahi/services/stratux.service
	# Copy the systemd units to the dpkg environment (canonical usrmerge path on Debian 12+)
	cp -f debian/stratux.service $(DEBPKG_BASE)/usr/lib/systemd/system/stratux.service
	cp -f debian/stratux-update.service $(DEBPKG_BASE)/usr/lib/systemd/system/stratux-update.service
	cp -f debian/stratux-ota-watchdog.service $(DEBPKG_BASE)/usr/lib/systemd/system/stratux-ota-watchdog.service
	cp -f debian/stratux-usb-gadget.service $(DEBPKG_BASE)/usr/lib/systemd/system/stratux-usb-gadget.service
	cp -f debian/stratux_fancontrol.service $(DEBPKG_BASE)/usr/lib/systemd/system/stratux_fancontrol.service
	chmod 0644 $(DEBPKG_BASE)/usr/lib/systemd/system/*.service
	# Set up version and arch inside DEBIAN/control
	sed -i 's/VERSION/$(VERSIONSTR)/g' $(DEBPKG_BASE)/DEBIAN/control
	sed -i 's/ARCH/$(ARCH)/g' $(DEBPKG_BASE)/DEBIAN/control
	# Set proper permissions for DEBIAN metadata and maintainer scripts
	chmod 0644 $(DEBPKG_BASE)/DEBIAN/control $(DEBPKG_BASE)/DEBIAN/conffiles
	chmod 0755 $(DEBPKG_BASE)/DEBIAN/preinst $(DEBPKG_BASE)/DEBIAN/postinst $(DEBPKG_BASE)/DEBIAN/prerm $(DEBPKG_BASE)/DEBIAN/postrm
	# Create the default US settings for the config default
	echo '{"UAT_Enabled": true,"OGN_Enabled": false,"DeveloperMode": false}' > $(DEBPKG_HOME)/cfg/stratux.conf.default
	chmod 0644 $(DEBPKG_HOME)/cfg/stratux.conf.default
	# Create the debian package with clean root:root ownership
	dpkg-deb --root-owner-group -b $(DEBPKG_BASE)
	# Rename the file and move it to the base directory. Include the arch in the name
	mv -f $(DEBPKG_BASE)/../stratux.deb ./stratux-$(VERSIONSTR)-$(ARCH).deb

clean:
	rm -rf $(CURDIR)/build
	rm -f stratuxrun libdump978.so fancontrol ahrs_approx *.deb
	[ ! -f dump1090/Makefile ] || make -C dump1090 clean
	[ ! -f dump978/Makefile ] || make -C dump978 clean
	[ ! -f rtl-ais/Makefile ] || make -C rtl-ais clean


# docker targets for ease of use
#
# NOTE: 'clean' works outside of docker
dall:
	./docker_run.sh "make all"

ddpkg:
	./docker_run.sh "make dpkg"
