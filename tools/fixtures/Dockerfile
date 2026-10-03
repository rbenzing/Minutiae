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

ENV SOURCE_DATE_EPOCH=1700000000 \
    TZ=UTC \
    LC_ALL=C
