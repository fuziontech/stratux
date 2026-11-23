#!/bin/bash
#
# sirf_config.sh: Script to set up BU-353-S4 receiver for Stratux.
# Resets receiver to 4800 baud and 1 Hz GPRMC messaging, then enables
# WAAS, 5 Hz position reporting, all NMEA messages needed by
# Stratux, and 38400 bps serial output.

set -euo pipefail

DEV="${1:-/dev/ttyUSB0}"

printf "About to configure SIRF receiver on %s.\n" "${DEV}"
printf "Press ctrl-C to abort or any other key to continue.\n"
read -r

# Iterate through common bitrates and send commands to reduce output to 1 Hz / 4800 bps.
printf "Setting SIRF and RPi baud rate of %s to 4800. Iterating through common rates.\n" "${DEV}"
printf "Current %s baudrate.\n" "${DEV}"
printf "\$PSRF103,00,7,00,0*22\r\n" > "${DEV}"
printf "\$PSRF100,1,4800,8,1,0*0E\r\n" > "${DEV}"
sleep 0.2

for baud in 38400 115200 57600 19200 9600; do
	printf "%s bps.\n" "${baud}"
	stty -F "${DEV}" "${baud}"
	printf "\$PSRF103,00,7,00,0*22\r\n" > "${DEV}"
	printf "\$PSRF100,1,4800,8,1,0*0E\r\n" > "${DEV}"
	sleep 0.2
done

stty -F "${DEV}" 4800
# GGA off:
printf "\$PSRF103,00,00,00,01*24\r\n" > "${DEV}"

# GLL off:
printf "\$PSRF103,01,00,00,01*27\r\n" > "${DEV}"

# GSA off:
printf "\$PSRF103,02,00,00,01*26\r\n" > "${DEV}"

# GSV off:
printf "\$PSRF103,03,00,00,01*27\r\n" > "${DEV}"

# RMC on:
printf "\$PSRF103,04,00,01,01*21\r\n" > "${DEV}"

# VTG off:
printf "\$PSRF103,05,00,00,01*21\r\n" > "${DEV}"

printf "SIRF device has been set to 4800 baud with RMC messages at 1 Hz.\n"
printf "Press ctrl-C to abort, or any other key to continue with setup.\n"
read -r

# Now start the Stratux setup.
printf "Sending Sirf PSRF100 command to set GPS baud rate to 38400\n"
printf "\$PSRF100,1,38400,8,1,0*3D\r\n" > "${DEV}"
sleep 0.2

printf "Resetting RPi baud rate of %s to 38400\n" "${DEV}"
stty -F "${DEV}" 38400
sleep 0.2
printf "Sending SIRF PSRF103 commands to configure NMEA message output\n"

# GGA:
printf "\$PSRF103,00,00,01,01*25\r\n" > "${DEV}"

# Uncomment next two commands to set GSA/GSV on each position message.
# Stratux doesn't need this much info - but keep for developer debug
# GSA (every position message):
#printf "\$PSRF103,02,00,01,01*27\r\n" > "${DEV}"
# GSV (every position message):
#printf "\$PSRF103,03,00,01,01*26\r\n" > "${DEV}"

# Next two commands set GSA/GSV on every 5th position message.
# Comment out (and uncomment above two commands) to report on
# every position.
# GSA (every 5 position messages):
printf "\$PSRF103,02,00,05,01*23\r\n" > "${DEV}"
# GSV (every 5 position messages):
printf "\$PSRF103,03,00,05,01*22\r\n" > "${DEV}"

# RMC:
printf "\$PSRF103,04,00,01,01*21\r\n" > "${DEV}"
# VTG:
printf "\$PSRF103,05,00,01,01*20\r\n" > "${DEV}"
sleep 0.2

printf "Sending SIRF PSRF151 command to enable WAAS\n"
printf "\$PSRF151,01*3F\r\n" > "${DEV}"
sleep 0.2

printf "Sending SIRF PSRF103 command to enable 5 Hz position reporting\n"
printf "\$PSRF103,00,6,00,0*23\r\n" > "${DEV}"

# Finally, test the connection.
printf "Opening %s to listen to GPS. Press ctrl-C to cancel.\n" "${DEV}"
cat "${DEV}"
