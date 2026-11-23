#!/bin/bash
#
# mtk_config.sh: Script to set up MTK3339 receiver for Stratux.
# Resets receiver to 9600 and 1 Hz GPRMC messaging, then enables
# WAAS, 5 Hz position reporting, the NMEA messages needed by
# Stratux, and 38400 bps serial output.

set -euo pipefail

DEV="${1:-/dev/ttyAMA0}"

printf "About to configure MTK3339 receiver on %s.\n" "${DEV}"
printf "Press ctrl-C to abort or any other key to continue.\n"
read -r

# Iterate through common bitrates and send commands to reduce output to 1 Hz / 9600 bps.
printf "Setting MTK and RPi baud rate of %s to 9600. Iterating through common rates.\n" "${DEV}"
printf "Current %s baudrate.\n" "${DEV}"
printf "\$PMTK220,1000*1F\r\n" > "${DEV}"
printf "\$PMTK251,9600*17\r\n" > "${DEV}"
sleep 0.2

for baud in 38400 115200 57600 19200 4800; do
	printf "%s bps.\n" "${baud}"
	stty -F "${DEV}" "${baud}"
	printf "\$PMTK220,1000*1F\r\n" > "${DEV}"
	printf "\$PMTK251,9600*17\r\n" > "${DEV}"
	sleep 0.2
done

stty -F "${DEV}" 9600
printf "\$PMTK314,0,1,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0*29\r\n" > "${DEV}"

printf "MTK has been set to 9600 baud with RMC messages at 1 Hz.\n"
printf "Press ctrl-C to abort, or any other key to continue with setup.\n"
read -r

# Now start the Stratux setup.
printf "Sending MTK command to set GPS baud rate to 38400\n"
printf "\$PMTK251,38400*27\r\n" > "${DEV}"
sleep 0.2

printf "Setting RPi baud rate of %s to 38400\n" "${DEV}"
stty -F "${DEV}" 38400
sleep 0.2
printf "Sending MTK command to configure NMEA message output\n"
printf "\$PMTK314,0,1,1,1,5,5,0,0,0,0,0,0,0,0,0,0,0,0,0*29\r\n" > "${DEV}"
sleep 0.2
printf "Sending MTK commands to enable WAAS\n"
printf "\$PMTK301,2*2E\r\n" > "${DEV}"
sleep 0.2
printf "\$PMTK513,1*28\r\n" > "${DEV}"
sleep 0.2
printf "Sending MTK commands to enable 5 Hz position reporting\n"
printf "\$PMTK220,200*2C\r\n" > "${DEV}"

# Finally, test the connection.
printf "Opening %s to listen to GPS. Press ctrl-C to cancel.\n" "${DEV}"
cat "${DEV}"
