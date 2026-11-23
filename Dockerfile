# Image for building Stratux
#
FROM golang:1.23-bookworm

ARG TARGETARCH

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        file \
        gcc \
        git \
        libncurses-dev \
        libusb-1.0-0-dev \
        make \
        nano \
        pkg-config \
        sudo \
        wget \
    && if [ "${TARGETARCH}" = "arm64" ] || [ "$(dpkg --print-architecture)" = "arm64" ]; then \
        wget -O /tmp/librtlsdr0_2.0.2-2_arm64.deb https://github.com/stratux/rtlsdr/releases/download/v1.0/librtlsdr0_2.0.2-2_arm64.deb \
        && wget -O /tmp/librtlsdr-dev_2.0.2-2_arm64.deb https://github.com/stratux/rtlsdr/releases/download/v1.0/librtlsdr-dev_2.0.2-2_arm64.deb \
        && dpkg -i /tmp/librtlsdr0_2.0.2-2_arm64.deb /tmp/librtlsdr-dev_2.0.2-2_arm64.deb \
        && rm -f /tmp/librtlsdr0_2.0.2-2_arm64.deb /tmp/librtlsdr-dev_2.0.2-2_arm64.deb; \
    else \
        apt-get install -y --no-install-recommends librtlsdr0 librtlsdr-dev; \
    fi \
    && rm -rf /var/lib/apt/lists/*

# specific to debian, ubuntu images come with user 'ubuntu' that is uid 1000
ENV USERNAME="stratux"
ENV USER_HOME=/home/$USERNAME

RUN useradd -m -d "$USER_HOME" -s /bin/bash "$USERNAME" \
    && echo "$USERNAME ALL=(ALL) NOPASSWD:ALL" >> /etc/sudoers

WORKDIR /data
