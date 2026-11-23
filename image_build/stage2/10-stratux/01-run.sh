#!/bin/bash -e

# install packages as part of the run portion of the script
# as they are necessary for the build.
#
# xx-packages will install to the PI so its not appropriate for build required packages

on_chroot << EOF
    apt update
    apt install --yes dnsmasq ifplugd iptables jq avahi-daemon curl minisign
    systemctl enable avahi-daemon || true

    # try to reduce writing to SD card as much as possible, so they don't get
    # bricked when yanking the power cable
    # Disable swap...
    systemctl disable dphys-swapfile
    apt purge -y dphys-swapfile

    systemctl disable dnsmasq # we start it manually on respective interfaces
    systemctl disable triggerhappy
    systemctl disable wpa_supplicant
    systemctl disable systemd-timesyncd # We sync time with GPS. Make sure there is no conflict if we have internet connection
    systemctl disable resize2fs_once

    systemctl disable apt-daily.timer
    systemctl disable apt-daily-upgrade.timer
    systemctl disable man-db.timer

    # Run DHCP on eth0 when cable is plugged in
    sed -i -e 's/INTERFACES=""/INTERFACES="eth0"/g' /etc/default/ifplugd

    # Do NOT bake shared SSH host private keys into release images.
    # Remove any build-time host keys and keep regenerate_ssh_host_keys enabled
    # so each device generates unique keys on first boot.
    rm -f /etc/ssh/ssh_host_*
    systemctl enable regenerate_ssh_host_keys || true

    # This is usually done by the console-setup service that takes quite long of first boot..
    /lib/console-setup/console-setup.sh

    # remove network-manager as Stratux depends on using ifupdown network interface
    apt remove -y network-manager
EOF

# install esptool
on_chroot << EOF
    apt install -y python3-pip
    pip install --break-system-packages esptool
EOF

# install bluez 5.82 to fix BLE advertising issues
# BlueZ 5.79 had limitations in peripheral mode, 5.82 is the latest confirmed to work with EU EFB software
BLUEZ_VERSION="5.82"
on_chroot << EOF
    cd /tmp
    
    # Install build dependencies
    apt install -y build-essential libdbus-1-dev libglib2.0-dev libudev-dev libical-dev libreadline-dev
    
    # Clone, build, and install BlueZ 5.82
    git clone --depth 1 --branch ${BLUEZ_VERSION} https://github.com/bluez/bluez.git bluez-src
    cd bluez-src
    ./configure --prefix=/usr --sysconfdir=/etc --localstatedir=/var --enable-library
    make -j$(nproc)
    make install
    
    # Cleanup
    cd ..
    rm -rf bluez-src
    
    # Remove build dependencies to save space
    apt autoremove -y build-essential
EOF

LIBRTLSDR_DEB="librtlsdr0_2.0.2-2_arm64.deb"
LIBRTLSDR_DEV_DEB="librtlsdr-dev_2.0.2-2_arm64.deb"
RTLSDR_DEB="rtl-sdr_2.0.2-2_arm64.deb"
on_chroot << EOF
    echo "Installing librtlsdr in chroot"
    cd /tmp
    apt install -y libusb-1.0-0 libusb-1.0-0-dev
    wget https://github.com/stratux/rtlsdr/releases/download/v1.0/${LIBRTLSDR_DEB}
    wget https://github.com/stratux/rtlsdr/releases/download/v1.0/${LIBRTLSDR_DEV_DEB}
    dpkg -i ${LIBRTLSDR_DEB}
    dpkg -i ${LIBRTLSDR_DEV_DEB}
    rm ${LIBRTLSDR_DEB}
    rm ${LIBRTLSDR_DEV_DEB}

    echo "Installing rtlsdr"
    cd /tmp
    wget https://github.com/stratux/rtlsdr/releases/download/v1.0/${RTLSDR_DEB}
    dpkg -i ${RTLSDR_DEB}
    rm ${RTLSDR_DEB}

    echo "Building and installing kalibrate-rtl"

    apt install --yes build-essential autoconf libtool libfftw3-dev git

    # kalibrate-rtl
    cd /tmp
    git clone https://github.com/steve-m/kalibrate-rtl
    cd kalibrate-rtl
    ./bootstrap
    ./configure
    make -j8
    make install
    cd ../
    rm -rf kalibrate-rtl

    # remove the dev package of rtlsdr
    dpkg -r librtlsdr-dev

    # remove now unused libusb-1.0-0-dev
    apt remove -y libusb-1.0-0-dev
EOF

# Prepare wiringpi for ogn trx via GPIO
WIRINGPI_FILENAME="wiringpi_3.14_arm64.deb"
WIRINGPI_FILENAME_TMP="${ROOTFS_DIR}/tmp/${WIRINGPI_FILENAME}"
wget -O "${WIRINGPI_FILENAME_TMP}" "https://github.com/WiringPi/WiringPi/releases/download/3.14/${WIRINGPI_FILENAME}"

# install the package
on_chroot << EOF
    dpkg -i "/tmp/${WIRINGPI_FILENAME}"
EOF

# remove the dpkg file from tmp
rm -f "${WIRINGPI_FILENAME_TMP}"


install -m 644 files/bashrc.txt "${ROOTFS_DIR}/root/.bashrc"

install -m 644 files/motd "${ROOTFS_DIR}/etc/motd"


# network default config. TODO: can't we just implement stratuxrun -write-network-config or something to generate them from template?
install -m 644 files/stratux-dnsmasq.conf "${ROOTFS_DIR}/etc/dnsmasq.d/stratux-dnsmasq.conf"

install -m 600 files/wpa_supplicant_ap.conf "${ROOTFS_DIR}/etc/wpa_supplicant/wpa_supplicant_ap.conf"
install -m 644 files/interfaces "${ROOTFS_DIR}/etc/network/interfaces"

# sshd config
install -m 644 files/sshd_config "${ROOTFS_DIR}/etc/ssh/sshd_config"

# Persist first-boot SSH host keys into the lower ext4 layer when overlay is active
install -d -m 755 "${ROOTFS_DIR}/etc/systemd/system/regenerate_ssh_host_keys.service.d"
cat << 'DROPIN_EOF' > "${ROOTFS_DIR}/etc/systemd/system/regenerate_ssh_host_keys.service.d/overlay.conf"
[Service]
ExecStartPost=/bin/sh -c 'if [ "$(findmnt -n -o FSTYPE /)" = "overlay" ]; then /sbin/overlayctl unlock && cp -a /etc/ssh/ssh_host_* /overlay/robase/etc/ssh/ && rm -f /overlay/robase/etc/systemd/system/multi-user.target.wants/regenerate_ssh_host_keys.service && sync && /sbin/overlayctl lock; fi'
DROPIN_EOF
chmod 644 "${ROOTFS_DIR}/etc/systemd/system/regenerate_ssh_host_keys.service.d/overlay.conf"

# debug aliases
install -m 644 files/stxAliases.txt "${ROOTFS_DIR}/root/.stxAliases"

# rtl-sdr setup
install -m 644 files/rtl-sdr-blacklist.conf "${ROOTFS_DIR}/etc/modprobe.d/"

# system tweaks
install -m 644 files/modules.txt "${ROOTFS_DIR}/etc/modules"

# boot settings
install -m 644 files/config.txt "${ROOTFS_DIR}/boot/firmware/"

# Enable USB-C OTG peripheral mode (USB-NCM tethered EFB networking) on Pi 4 / Pi 5.
# On Pi 4/5 the USB-C OTG port is on dwc2 while USB-A host ports remain on xHCI,
# so USB-A SDR dongles and GPS receivers continue working normally. Pi 3 stays in host mode.
if ! grep -q 'dr_mode=peripheral' "${ROOTFS_DIR}/boot/firmware/config.txt"; then
    cat << 'USB_CFG_EOF' >> "${ROOTFS_DIR}/boot/firmware/config.txt"

# USB-C tethered EFB networking (USB-NCM gadget) on Pi 4 / Pi 5 OTG port.
# USB-A host ports remain on xHCI. Comment out to revert the USB-C port to host mode.
[pi4]
dtoverlay=dwc2,dr_mode=peripheral
[pi5]
dtoverlay=dwc2,dr_mode=peripheral
[all]
USB_CFG_EOF
fi

# rootfs overlay stuff
install -m 755 files/overlayctl files/init-overlay "${ROOTFS_DIR}/sbin/"

on_chroot << EOF
    overlayctl install
    # init-overlay replaces raspis initial partition size growing.. Make sure we call that manually (see init-overlay script)
    touch /var/grow_root_part
    mkdir -p /overlay/robase # prepare so we can bind-mount root even if overlay is disabled
EOF

# So we can import network settings if needed
touch "${ROOTFS_DIR}/boot/firmware/.stratux-first-boot"

# startup scripts
install -m 755 files/rc.local "${ROOTFS_DIR}/etc/rc.local"

# Optionally mount /dev/sda1 as /var/log - for logging to USB stick (idempotent)
if ! grep -q '^/dev/sda1[[:space:]]\+/var/log[[:space:]]' "${ROOTFS_DIR}/etc/fstab"; then
    printf '\n/dev/sda1             /var/log        auto    defaults,nofail,noatime,nodev,nosuid,noexec,x-systemd.device-timeout=2s  0       2\n' >> "${ROOTFS_DIR}/etc/fstab"
fi

# disable serial console, disable rfkill state restore, enable wifi on boot
sed -i ${ROOTFS_DIR}/boot/firmware/cmdline.txt -e "s/console=serial0,[0-9]\+ /systemd.restore_state=0 rfkill.default_state=1 /"
sed -i 's/quiet//g' ${ROOTFS_DIR}/boot/firmware/cmdline.txt

# Set the keyboard layout to US.
sed -i ${ROOTFS_DIR}/etc/default/keyboard -e "/^XKBLAYOUT/s/\".*\"/\"us\"/"

# Set hostname
echo "stratux" > ${ROOTFS_DIR}/etc/hostname
sed -i ${ROOTFS_DIR}/etc/hosts -e "s/raspberrypi/stratux/g"
