FROM golang:1.26-trixie AS builder

ENV GOFLAGS="-mod=readonly"

RUN apt-get update && apt-get -y upgrade && rm -rf /var/lib/apt/lists/*

RUN mkdir -p /workspace
WORKDIR /workspace

ARG GOPROXY

COPY go.mod go.sum ./
RUN go mod download && go mod verify

# Fork: download the GeoNames gazetteer used to name the places where photos
# were taken (see FORK_FEATURES.md, Places). Data by GeoNames, CC BY 4.0.
# Set to "false" to skip it: photos are still shown on the map, without names.
# A failed download only prints a warning.
ARG INSTALL_GEONAMES=true

RUN mkdir -p /workspace/geonames && if [ "${INSTALL_GEONAMES}" = "true" ]; then \
      cd /workspace/geonames && \
      for f in cities500.zip admin1CodesASCII.txt countryInfo.txt; do \
        curl -fsSL --retry 3 -o "$f" "https://download.geonames.org/export/dump/$f" || { echo "WARNING: unable to download $f, places will have no names"; rm -f /workspace/geonames/*; break; }; \
      done; \
    fi

ARG COMMIT_SHA

# This ARG allows to disable some optional features and it might be useful if you build the image yourself.
# For example you can disable S3 and GCS support like this:
# --build-arg FEATURES=nos3,nogcs
ARG FEATURES

COPY . .

RUN set -xe && \
    export COMMIT_SHA=${COMMIT_SHA:-$(git describe --always --abbrev=8 --dirty)} && \
    go build $(if [ -n "${FEATURES}" ]; then echo "-tags ${FEATURES}"; fi) -trimpath -ldflags "-s -w -X github.com/drakkan/sftpgo/v2/internal/version.commit=${COMMIT_SHA} -X github.com/drakkan/sftpgo/v2/internal/version.date=`date -u +%FT%TZ`" -v -o sftpgo

# Set to "true" to download the "official" plugins in /usr/local/bin
ARG DOWNLOAD_PLUGINS=false

RUN if [ "${DOWNLOAD_PLUGINS}" = "true" ]; then apt-get update && apt-get install --no-install-recommends -y curl && ./docker/scripts/download-plugins.sh; fi

FROM debian:trixie-slim

# Set to "true" to install jq
ARG INSTALL_OPTIONAL_PACKAGES=false

# Fork: install ffmpeg so the WebClient can generate video thumbnails
# (see FORK_FEATURES.md). Set to "false" to skip it and save image size; image
# thumbnails still work and videos fall back to a generic icon.
ARG INSTALL_FFMPEG=true

# Fork: install libvips (with HEIC support) and exiftool for the photo index
# (see FORK_FEATURES.md). Set to "false" to skip them: the photo index then
# derives dates from file names only and generates no HEIC/RAW previews.
ARG INSTALL_PHOTO_TOOLS=true

RUN apt-get update && apt-get -y upgrade && apt-get install --no-install-recommends -y ca-certificates media-types && rm -rf /var/lib/apt/lists/*

RUN if [ "${INSTALL_OPTIONAL_PACKAGES}" = "true" ]; then apt-get update && apt-get install --no-install-recommends -y jq && rm -rf /var/lib/apt/lists/*; fi

RUN if [ "${INSTALL_FFMPEG}" = "true" ]; then apt-get update && apt-get install --no-install-recommends -y ffmpeg && rm -rf /var/lib/apt/lists/*; fi

RUN if [ "${INSTALL_PHOTO_TOOLS}" = "true" ]; then apt-get update && apt-get install --no-install-recommends -y libvips-tools libheif-plugin-libde265 libimage-exiftool-perl && rm -rf /var/lib/apt/lists/*; fi

RUN mkdir -p /etc/sftpgo /var/lib/sftpgo/photoindex /usr/share/sftpgo /srv/sftpgo/data /srv/sftpgo/backups

RUN groupadd --system -g 1000 sftpgo && \
    useradd --system --gid sftpgo --no-create-home \
    --home-dir /var/lib/sftpgo --shell /usr/sbin/nologin \
    --comment "SFTPGo user" --uid 1000 sftpgo

COPY --from=builder /workspace/sftpgo.json /etc/sftpgo/sftpgo.json
COPY --from=builder /workspace/templates /usr/share/sftpgo/templates
COPY --from=builder /workspace/static /usr/share/sftpgo/static
COPY --from=builder /workspace/openapi /usr/share/sftpgo/openapi
COPY --from=builder /workspace/geonames /usr/share/sftpgo/geonames
COPY --from=builder /workspace/sftpgo /usr/local/bin/sftpgo-plugin-* /usr/local/bin/

# Log to the stdout so the logs will be available using docker logs
ENV SFTPGO_LOG_FILE_PATH=""

# Modify the default configuration file
RUN sed -i 's|"users_base_dir": "",|"users_base_dir": "/srv/sftpgo/data",|' /etc/sftpgo/sftpgo.json && \
    sed -i 's|"backups"|"/srv/sftpgo/backups"|' /etc/sftpgo/sftpgo.json

RUN chown -R sftpgo:sftpgo /etc/sftpgo /srv/sftpgo && chown sftpgo:sftpgo /var/lib/sftpgo /var/lib/sftpgo/photoindex && chmod 700 /srv/sftpgo/backups

WORKDIR /var/lib/sftpgo
USER 1000:1000

CMD ["sftpgo", "serve"]
