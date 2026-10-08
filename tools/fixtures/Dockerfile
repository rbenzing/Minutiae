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

# sqlite3 (the command-line shell, with libsqlite3 that python3's sqlite3 module
# links) for the SQLite fixtures: the engine that writes the container family
# and the oracle that checks every fixture. A separate, final layer so every
# layer above, and every fixture generated from it, stays as built.
# The exact package versions are pinned (the engine is part of the oracle) and
# checked after the install; bump them together with a regeneration of the class
# C fixtures. Their expect.json files record libsqlite3 3.46.1.
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      sqlite3=3.46.1-7+deb13u2 libsqlite3-0=3.46.1-7+deb13u2 \
 && rm -rf /var/lib/apt/lists/* \
 && test "$(dpkg-query -W -f '${Version}' sqlite3)" = "3.46.1-7+deb13u2" \
 && test "$(dpkg-query -W -f '${Version}' libsqlite3-0)" = "3.46.1-7+deb13u2" \
 && sqlite3 --version | grep -q '^3\.46\.1 '
