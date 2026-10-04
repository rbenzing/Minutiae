# Toolchain used to generate the real-image test fixtures (see README.md).
# Generating fixtures needs Docker; running the Go tests never does.
FROM debian:stable-slim

# hfsprogs has no installation candidate in current debian stable; the HFS+
# fixture task must supply an HFS+ formatter itself.
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      e2fsprogs dosfstools mtools exfatprogs f2fs-tools ewf-tools \
      gdisk fdisk util-linux jq gzip coreutils python3 \
 && rm -rf /var/lib/apt/lists/*

# faketime freezes the clock for mtools (it stamps created/accessed times from
# the clock); exfat-fuse is the only way to populate an exFAT image here (the
# Docker Desktop kernel has no exfat driver; exfatprogs cannot copy files in).
# A separate layer so the layer above, and the e2fsprogs it pins, stay as built.
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      faketime exfat-fuse \
 && rm -rf /var/lib/apt/lists/*

ENV SOURCE_DATE_EPOCH=1700000000 \
    TZ=UTC \
    LC_ALL=C

# apfsprogs (mkapfs, apfsck) for the APFS fixtures. A separate, final layer so
# every layer above, and every fixture generated from it, stays as built.
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      apfsprogs \
 && rm -rf /var/lib/apt/lists/*
