#!/bin/bash
# Feasibility probe of plan 3B: what do the KERNEL vfat and exfat drivers leave
# behind when a file is deleted, and in what order do they allocate clusters?
# Run inside the minutiae-recover-fixtures image (README.md, "FAT/exFAT recovery
# probe (3B)"):
#   docker run --rm -v "$PWD:/work" -w /work minutiae-recover-fixtures bash tools/fixtures/recover_fat_probe.sh
# No --privileged, no FUSE, no loop devices, no host mounts. Every write and
# delete runs in the QEMU TCG guest (recover_guest.sh); the host only formats
# the images (under faketime) and READS the finished images with a raw reader.
# Output: "IMAGE"/"ENTRY" listings, "HEX" dumps and "FACT-RAW" lines.
set -eu
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=lib.sh
. "$here/lib.sh"
# shellcheck source=recover_guest.sh
. "$here/recover_guest.sh"

work=$(mktemp -d /tmp/recover-fat-probe.XXXXXX)
trap 'rm -rf "$work"' EXIT
out=$work/img; mkdir -p "$out" "$work/payload"
ft="2023-11-14 22:13:20"

# payload files (cluster size is 512 on both volumes): A 6, B 4, C 8, D 4, E 6 clusters,
# F = A + C = 14 clusters (it must span the two holes), W and X for the exFAT length probes
pattern 3000 A >"$work/payload/A.BIN"
pattern 2000 B >"$work/payload/B.BIN"
pattern 4000 C >"$work/payload/C.BIN"
pattern 2000 D >"$work/payload/D.BIN"
pattern 3000 E >"$work/payload/E.BIN"
pattern 7000 F >"$work/payload/F.BIN"
pattern 100 W >"$work/payload/W.BIN"
pattern 512 X >"$work/payload/X.BIN"

# stage <fs> <stage name> <mount opts>: the ops come on stdin, the guest boots on
# the previous stage's image and the result is kept as <fs>-<stage>.img
stage() {
  local fs=$1 name=$2 opts=$3
  cat >"$work/ops"
  guest_initrd "$work/rd" "$fs" "$opts" "$work/payload" "$work/ops"
  guest_run "$work/$fs.img" "$work/rd" "$work/guest-$fs-$name.log"
  cp "$work/$fs.img" "$out/$fs-$name.img"
}

echo "== formatting (host side, faketime $ft)"
truncate -s 4M "$work/vfat.img"
faketime -f "$ft" mkfs.fat -F 16 -s 1 -i 1234ABCD "$work/vfat.img" >/dev/null
truncate -s 8M "$work/exfat.img"
faketime -f "$ft" mkfs.exfat -L PROBE -c 512 "$work/exfat.img" >/dev/null
cp "$work/vfat.img" "$out/vfat-s0.img"; cp "$work/exfat.img" "$out/exfat-s0.img"

for fs in vfat exfat; do
  echo "== guest stages for $fs"
  cp "$out/$fs-s0.img" "$work/$fs.img"
  stage $fs s1w "" <<'OPS'
cp /payload/A.BIN /mnt/A.BIN; sync
cp /payload/B.BIN /mnt/B.BIN; sync
cp /payload/C.BIN /mnt/C.BIN; sync
cp /payload/D.BIN /mnt/D.BIN; sync
OPS
  stage $fs s2d "" <<'OPS'
rm /mnt/B.BIN /mnt/D.BIN; sync
OPS
  stage $fs s3w "" <<'OPS'
cp /payload/E.BIN /mnt/E.BIN; sync
OPS
  stage $fs s4f "" <<'OPS'
dd if=/dev/zero of=/mnt/FILL.BIN bs=512 2>/dev/null || true
sync
OPS
  stage $fs s5d "" <<'OPS'
rm /mnt/A.BIN /mnt/C.BIN; sync
OPS
  stage $fs s6w "" <<'OPS'
cp /payload/F.BIN /mnt/F.BIN; sync
OPS
  stage $fs s7d "" <<'OPS'
rm /mnt/F.BIN; sync
OPS
done
for fs in vfat exfat; do
  echo "== guest stage $fs: allocation inside ONE mount (next-fit or first-fit?)"
  cp "$out/$fs-s0.img" "$work/$fs.img"
  stage $fs s9n "" <<'OPS'
cp /payload/A.BIN /mnt/A.BIN; cp /payload/B.BIN /mnt/B.BIN; cp /payload/C.BIN /mnt/C.BIN; sync
rm /mnt/B.BIN; sync
cp /payload/D.BIN /mnt/D.BIN; sync
cp /payload/E.BIN /mnt/E.BIN; sync
OPS
done
echo "== guest stage for exfat length probes (P5)"
# fresh volume: a file extended by truncate, a short file extended by truncate,
# a file whose first write starts past the end (dd seek), a seek with no write
cp "$out/exfat-s0.img" "$work/exfat.img"
stage exfat s8v "" <<'OPS'
: > /mnt/T1.BIN; truncate -s 5000 /mnt/T1.BIN; sync
cp /payload/W.BIN /mnt/T2.BIN; truncate -s 4000 /mnt/T2.BIN; sync
dd if=/payload/X.BIN of=/mnt/T3.BIN bs=512 seek=6 count=1 2>/dev/null; sync
dd if=/dev/zero of=/mnt/T4.BIN bs=512 seek=8 count=0 2>/dev/null; sync
OPS

echo "== host-side read of the finished images (read-only)"
cat >"$work/read.py" <<'PY'
import binascii, struct, sys


def hx(label, b):
    print("HEX %-40s %s" % (label, binascii.hexlify(b).decode()))


class Fat16:
    def __init__(self, path):
        self.d = open(path, "rb").read()
        d = self.d
        self.bps, self.spc, self.res, self.nf = struct.unpack_from("<HBHB", d, 11)
        self.rootents = struct.unpack_from("<H", d, 17)[0]
        self.fatsz = struct.unpack_from("<H", d, 22)[0]
        tot = struct.unpack_from("<H", d, 19)[0] or struct.unpack_from("<I", d, 32)[0]
        self.fat0 = self.res * self.bps
        self.root = (self.res + self.nf * self.fatsz) * self.bps
        self.data = self.root + self.rootents * 32
        self.ncl = (tot * self.bps - self.data) // (self.spc * self.bps)
        self.last = self.ncl + 1
        self.cs = self.spc * self.bps

    def fat(self, c):
        return struct.unpack_from("<H", self.d, self.fat0 + 2 * c)[0]

    def fat_bytes(self, c):
        return self.d[self.fat0 + 2 * c: self.fat0 + 2 * c + 2]

    def free(self):
        return {c for c in range(2, self.last + 1) if self.fat(c) == 0}

    def chain(self, start, size, nofat=False):
        out, c = [], start
        while 2 <= c <= self.last and len(out) <= self.ncl:
            out.append(c)
            n = self.fat(c)
            if n >= 0xFFF8:
                break
            c = n
        return out

    def entries(self):
        res = []
        for i in range(self.rootents):
            e = self.d[self.root + 32 * i: self.root + 32 * i + 32]
            if e[0] == 0:
                break
            if e[11] == 0x0F:
                continue
            start = struct.unpack_from("<H", e, 26)[0]
            size = struct.unpack_from("<I", e, 28)[0]
            deleted = e[0] == 0xE5
            name = (b"?" + e[1:8] if deleted else e[0:8]).decode("latin1").strip() + "." + e[8:11].decode("latin1").strip()
            res.append(dict(name=name, deleted=deleted, start=start, size=size, raw=bytes(e), nofat=False, vdl=size))
        return res


class Exfat:
    def __init__(self, path):
        self.d = open(path, "rb").read()
        d = self.d
        self.bps = 1 << d[108]
        self.spc = 1 << d[109]
        self.fatoff, self.fatlen, self.heap, self.ncl, self.rootc = struct.unpack_from("<IIIII", d, 80)
        self.cs = self.bps * self.spc
        self.last = self.ncl + 1
        self.bmstart = None
        self.bmsize = None

    def coff(self, c):
        return self.heap * self.bps + (c - 2) * self.cs

    def fat(self, c):
        return struct.unpack_from("<I", self.d, self.fatoff * self.bps + 4 * c)[0]

    def fat_bytes(self, c):
        o = self.fatoff * self.bps + 4 * c
        return self.d[o: o + 4]

    def chain(self, start, size, nofat=False):
        if nofat:
            n = -(-size // self.cs)
            return list(range(start, start + n))
        out, c = [], start
        while 2 <= c <= self.last and len(out) <= self.ncl:
            out.append(c)
            n = self.fat(c)
            if n >= 0xFFFFFFF8:
                break
            c = n
        return out

    def bit(self, c):
        o = self.coff(self.bmstart) + (c - 2) // 8
        return (self.d[o] >> ((c - 2) % 8)) & 1

    def bitmap_byte(self, c):
        o = self.coff(self.bmstart) + (c - 2) // 8
        return self.d[o: o + 1]

    def free(self):
        return {c for c in range(2, self.last + 1) if not self.bit(c)}

    def entries(self):
        res = []
        raw = b""
        for c in self.chain(self.rootc, 0):
            raw += self.d[self.coff(c): self.coff(c) + self.cs]
        i = 0
        while i + 32 <= len(raw):
            t = raw[i]
            if t == 0:
                break
            if t == 0x81:
                self.bmstart = struct.unpack_from("<I", raw, i + 20)[0]
                self.bmsize = struct.unpack_from("<Q", raw, i + 24)[0]
            if t in (0x85, 0x05):
                sc = raw[i + 1]
                sset = raw[i: i + 32 * (sc + 1)]
                st = sset[32:64]
                nm = b"".join(sset[32 * k + 2: 32 * k + 32] for k in range(2, sc + 1))
                name = nm.decode("utf-16-le", "replace")[: st[3]]
                res.append(dict(name=name, deleted=(t == 0x05), start=struct.unpack_from("<I", st, 20)[0],
                                size=struct.unpack_from("<Q", st, 24)[0], vdl=struct.unpack_from("<Q", st, 8)[0],
                                nofat=bool(st[1] & 2), raw=bytes(sset), types=[sset[32 * k] for k in range(sc + 1)]))
                i += 32 * (sc + 1)
                continue
            i += 32
        return res


def load(fs, path):
    f = Fat16(path) if fs == "vfat" else Exfat(path)
    if fs == "exfat":
        f.rootents = None
    f.ents = f.entries()
    return f


def live_chain(f, e):
    if e["deleted"]:
        return None
    return f.chain(e["start"], e["size"], e["nofat"])


def find(f, name):
    for e in f.ents:
        if e["name"].upper() == name:
            return e
    return None


def rng(cl):
    cl = sorted(cl) if isinstance(cl, set) else cl
    if not cl:
        return "[]"
    runs, s, p = [], cl[0], cl[0]
    for c in cl[1:]:
        if c != p + 1:
            runs.append((s, p))
            s = c
        p = c
    runs.append((s, p))
    return ",".join("%d-%d" % r if r[0] != r[1] else str(r[0]) for r in runs)


def show(fs, stage, f):
    print("IMAGE %s-%s clusters=%d cs=%d free=%d" % (fs, stage, f.ncl, f.cs, len(f.free())))
    for e in f.ents:
        cl = live_chain(f, e)
        print("  ENTRY %-9s deleted=%s start=%d size=%d vdl=%d nofatchain=%s chain=%s" % (
            e["name"], e["deleted"], e["start"], e["size"], e["vdl"], e["nofat"], rng(cl) if cl is not None else "(deleted)"))


def first_fit(f_before, cl, skip2=False):
    # does the new file occupy exactly the lowest-numbered free clusters of the previous image?
    free = sorted(f_before.free() - ({2} if skip2 else set()))
    return sorted(cl) == free[: len(cl)]


fs, d = sys.argv[1], sys.argv[2]
st = {s: load(fs, "%s/%s-%s.img" % (d, fs, s)) for s in ("s0", "s1w", "s2d", "s3w", "s4f", "s5d", "s6w", "s7d")}
for s, f in st.items():
    show(fs, s, f)
tag = "P1" if fs == "vfat" else "P3"

# deleted files: contiguous B and D (s1w -> s2d), fragmented F (s6w -> s7d)
for label, bef, aft, name in (("contiguous B", "s1w", "s2d", "B.BIN"), ("contiguous D", "s1w", "s2d", "D.BIN"), ("fragmented F", "s6w", "s7d", "F.BIN")):
    fb, fa = st[bef], st[aft]
    eb = find(fb, name)
    cl = live_chain(fb, eb)
    ea = None
    for e in fa.ents:
        if e["deleted"] and e["start"] == eb["start"] and e["size"] == eb["size"]:
            ea = e
    print("== %s (%s): clusters before = %s" % (label, name, rng(cl)))
    hx("%s dirent/entry-set BEFORE" % name, eb["raw"])
    if ea is not None:
        hx("%s dirent/entry-set AFTER" % name, ea["raw"])
    fvb, fva = [fb.fat(c) for c in cl], [fa.fat(c) for c in cl]
    print("  FAT values before: %s" % fvb)
    print("  FAT values after : %s" % fva)
    hx("FAT bytes before (clusters %s)" % rng(cl), b"".join(fb.fat_bytes(c) for c in cl))
    hx("FAT bytes after", b"".join(fa.fat_bytes(c) for c in cl))
    print("  FACT-RAW %s %s: entry_after_found=%s fat_zeroed=%s fat_unchanged=%s" % (tag, label, ea is not None, all(v == 0 for v in fva), fva == fvb))
    if fs == "vfat":
        print("  FACT-RAW P1 %s: dirent first byte after=0x%02x start_kept=%s size_kept=%s" % (
            label, ea["raw"][0] if ea else 0, bool(ea) and ea["start"] == eb["start"], bool(ea) and ea["size"] == eb["size"]))
    else:
        bb = [fb.bit(c) for c in cl]
        ba = [fa.bit(c) for c in cl]
        hx("bitmap byte before (cluster %d)" % cl[0], fb.bitmap_byte(cl[0]))
        hx("bitmap byte after  (cluster %d)" % cl[0], fa.bitmap_byte(cl[0]))
        print("  FACT-RAW P3 %s: nofatchain_before=%s bits_before_all_set=%s bits_after_all_clear=%s types_before=%s types_after=%s start_kept=%s size_kept=%s vdl_kept=%s" % (
            label, eb["nofat"], all(bb), not any(ba), [hex(t) for t in eb["types"]],
            [hex(t) for t in ea["types"]] if ea else None, bool(ea) and ea["start"] == eb["start"], bool(ea) and ea["size"] == eb["size"],
            bool(ea) and ea["vdl"] == eb["vdl"]))

# allocation order (P2 for vfat, P4 for exfat)
ptag = "P2" if fs == "vfat" else "P4"
a, b, c, dd = [live_chain(st["s1w"], find(st["s1w"], n)) for n in ("A.BIN", "B.BIN", "C.BIN", "D.BIN")]
print("== allocation order %s" % ptag)
print("  empty volume, files written A,B,C,D: A=%s B=%s C=%s D=%s" % (rng(a), rng(b), rng(c), rng(dd)))
asc = all(x[-1] < y[0] for x, y in ((a, b), (b, c), (c, dd)))
print("  FACT-RAW %s empty volume, written A,B,C,D: each file after the previous, contiguous: %s" % (ptag, asc))
e = live_chain(st["s3w"], find(st["s3w"], "E.BIN"))
print("  after rm B,D (holes %s and %s) E (6 clusters) = %s" % (rng(b), rng(dd), rng(e)))
print("  FACT-RAW %s E takes the lowest free clusters: %s ; the lowest free clusters excluding cluster 2: %s ; E inside the holes B+D: %s" % (ptag, first_fit(st["s2d"], e), first_fit(st["s2d"], e, True), set(e) <= set(b) | set(dd)))
fre = st["s4f"]
print("  after fill to ENOSPC: free clusters = %d ; FILL chain = %s" % (len(fre.free()), rng(live_chain(fre, find(fre, "FILL.BIN")))))
print("  FACT-RAW P6 free clusters after dd to ENOSPC: %d" % len(fre.free()))
a2 = st["s5d"].free()
print("  after rm A,C on the full volume: free set = %s (A=%s C=%s)" % (rng(a2), rng(a), rng(c)))
print("  FACT-RAW P6 free set after rm A,C equals the A+C clusters exactly: %s" % (a2 == set(a) | set(c)))
fcl = live_chain(st["s6w"], find(st["s6w"], "F.BIN"))
print("  F (14 clusters) written into the A,C holes: chain in order = %s" % fcl)
print("  FACT-RAW %s F chain runs: %s ; F ascending: %s ; F fills exactly the holes: %s" % (
    ptag, rng(fcl), fcl == sorted(fcl), set(fcl) == set(a) | set(c)))

n = load(fs, "%s/%s-s9n.img" % (d, fs))
show(fs, "s9n", n)
na, nb = live_chain(n, find(n, "A.BIN")), None
nd = live_chain(n, find(n, "D.BIN"))
nc = live_chain(n, find(n, "C.BIN"))
ne = live_chain(n, find(n, "E.BIN"))
print("  one mount: A,B,C written, rm B, then D (4 clusters, B's old size) and E (6) written: D=%s E=%s C=%s" % (rng(nd), rng(ne), rng(nc)))
print("  FACT-RAW %s within one mount D lands after C (next-fit), not in B's hole: %s ; first allocated cluster of a fresh volume: %d" % (
    ptag, min(nd) > max(nc), min(live_chain(n, find(n, "A.BIN")))))

if fs == "exfat":
    f = load(fs, "%s/exfat-s8v.img" % d)
    show(fs, "s8v", f)
    for e in f.ents:
        if e["name"].startswith("T"):
            hx("%s entry set" % e["name"], e["raw"])
            print("  FACT-RAW P5 %s: DataLength=%d ValidDataLength=%d vdl_less_than_size=%s" % (e["name"], e["size"], e["vdl"], e["vdl"] < e["size"]))
PY
for fs in vfat exfat; do
  echo "######## $fs"
  python3 -I "$work/read.py" $fs "$out"
done
echo "== independent tool cross-checks"
fsck.fat -n -v "$out/vfat-s7d.img" 2>&1 | grep -iE 'free|bytes per cluster|FATs|root' | head -5 || true
for s in s1w s2d; do
  echo "-- mshowfat B.BIN in vfat-$s"
  MTOOLS_SKIP_CHECK=1 mshowfat -i "$out/vfat-$s.img" ::B.BIN 2>&1 | head -2 || true
done
dump.exfat "$out/exfat-s4f.img" 2>&1 | grep -iE 'free clusters|cluster count|cluster size' || true
echo "PROBE COMPLETE"
