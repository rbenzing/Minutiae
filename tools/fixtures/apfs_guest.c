/*
 * apfs_guest: the few operations the populate guest needs that busybox cannot
 * do (see apfs_populate.sh). Built statically by the generator and run inside
 * the QEMU guest, on the freshly mounted APFS volume.
 *
 * usage: apfs_guest <ops file>
 *
 * Each line of the ops file is one operation, fields separated by TABs
 * (names may hold spaces, never TABs or newlines; values are hex):
 *
 *   chmod   <path> <octal mode>
 *   chown   <path> <uid> <gid>
 *   utime   <path> <sec> <nsec>        atime = mtime, symlinks not followed
 *   xattr   <path> <name> <hex value>
 *   pwrite  <path> <offset> <hex data> creates the file when missing
 *   truncate <path> <size>
 *   append  <path> <hex data>          O_APPEND write, then fsync (the driver allocates at once)
 *   clone   <src> <dst> <octal mode>   whole-file reflink (FICLONE)
 *   rm      <path>
 *   sync
 *   snap    <mount point> <name>       the volume's snapshot ioctl
 *
 * It stops at the first failure with a message the init script turns into
 * MINUTIAE-GUEST-FAIL.
 */
#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <sys/ioctl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/types.h>
#include <sys/xattr.h>
#include <unistd.h>

#define FICLONE _IOW(0x94, 9, int)
struct apfs_ioctl_snap_name {
	char name[256];
};
#define APFS_IOC_TAKE_SNAPSHOT _IOW('@', 0x85, struct apfs_ioctl_snap_name)

static void die(const char *op, const char *path)
{
	fprintf(stderr, "apfs_guest: %s %s: %s\n", op, path, strerror(errno));
	exit(1);
}

static unsigned char *unhex(const char *s, size_t *n)
{
	size_t len = strlen(s), i;
	unsigned char *b = malloc(len / 2 + 1);

	if (!b || len % 2) {
		fprintf(stderr, "apfs_guest: bad hex value\n");
		exit(1);
	}
	for (i = 0; i < len / 2; i++) {
		unsigned int v;

		if (sscanf(s + 2 * i, "%2x", &v) != 1) {
			fprintf(stderr, "apfs_guest: bad hex digit\n");
			exit(1);
		}
		b[i] = (unsigned char)v;
	}
	*n = len / 2;
	return b;
}

int main(int argc, char **argv)
{
	FILE *f;
	char *line = NULL;
	size_t cap = 0;
	ssize_t got;
	long lineno = 0;

	if (argc != 2) {
		fprintf(stderr, "usage: apfs_guest <ops file>\n");
		return 2;
	}
	f = fopen(argv[1], "r");
	if (!f)
		die("open", argv[1]);
	while ((got = getline(&line, &cap, f)) > 0) {
		char *fld[5] = {0};
		int nf = 0;
		char *p = line;

		lineno++;
		if (line[got - 1] == '\n')
			line[--got] = 0;
		if (!*line)
			continue;
		while (nf < 5) {
			fld[nf++] = p;
			p = strchr(p, '\t');
			if (!p)
				break;
			*p++ = 0;
		}
		if (!strcmp(fld[0], "chmod") && nf == 3) {
			if (chmod(fld[1], (mode_t)strtoul(fld[2], NULL, 8)))
				die("chmod", fld[1]);
		} else if (!strcmp(fld[0], "chown") && nf == 4) {
			if (lchown(fld[1], (uid_t)atol(fld[2]), (gid_t)atol(fld[3])))
				die("chown", fld[1]);
		} else if (!strcmp(fld[0], "utime") && nf == 4) {
			struct timespec ts[2];

			ts[0].tv_sec = ts[1].tv_sec = atol(fld[2]);
			ts[0].tv_nsec = ts[1].tv_nsec = atol(fld[3]);
			if (utimensat(AT_FDCWD, fld[1], ts, AT_SYMLINK_NOFOLLOW))
				die("utime", fld[1]);
		} else if (!strcmp(fld[0], "xattr") && nf == 4) {
			size_t n;
			unsigned char *v = unhex(fld[3], &n);

			if (lsetxattr(fld[1], fld[2], v, n, XATTR_CREATE))
				die("setxattr", fld[1]);
			free(v);
		} else if (!strcmp(fld[0], "pwrite") && nf == 4) {
			size_t n;
			unsigned char *v = unhex(fld[3], &n);
			int fd = open(fld[1], O_WRONLY | O_CREAT, 0644);

			if (fd < 0)
				die("open", fld[1]);
			if (pwrite(fd, v, n, (off_t)atoll(fld[2])) != (ssize_t)n)
				die("pwrite", fld[1]);
			if (close(fd))
				die("close", fld[1]);
			free(v);
		} else if (!strcmp(fld[0], "append") && nf == 3) {
			size_t n;
			unsigned char *v = unhex(fld[2], &n);
			int fd = open(fld[1], O_WRONLY | O_CREAT | O_APPEND, 0644);

			if (fd < 0)
				die("open", fld[1]);
			if (write(fd, v, n) != (ssize_t)n)
				die("write", fld[1]);
			if (fsync(fd))
				die("fsync", fld[1]);
			if (close(fd))
				die("close", fld[1]);
			free(v);
		} else if (!strcmp(fld[0], "truncate") && nf == 3) {
			if (truncate(fld[1], (off_t)atoll(fld[2])))
				die("truncate", fld[1]);
		} else if (!strcmp(fld[0], "clone") && nf == 4) {
			int s = open(fld[1], O_RDONLY);
			int d = open(fld[2], O_WRONLY | O_CREAT | O_EXCL, (mode_t)strtoul(fld[3], NULL, 8));

			if (s < 0)
				die("open", fld[1]);
			if (d < 0)
				die("create", fld[2]);
			if (ioctl(d, FICLONE, s))
				die("FICLONE", fld[2]);
			close(s);
			if (close(d))
				die("close", fld[2]);
		} else if (!strcmp(fld[0], "rm") && nf == 2) {
			if (unlink(fld[1]))
				die("unlink", fld[1]);
		} else if (!strcmp(fld[0], "sync") && nf == 1) {
			sync();
		} else if (!strcmp(fld[0], "snap") && nf == 3) {
			struct apfs_ioctl_snap_name arg;
			int fd = open(fld[1], O_RDONLY | O_DIRECTORY);

			memset(&arg, 0, sizeof(arg));
			if (strlen(fld[2]) > 255) {
				fprintf(stderr, "apfs_guest: snapshot name too long\n");
				exit(1);
			}
			strcpy(arg.name, fld[2]);
			if (fd < 0)
				die("open", fld[1]);
			if (ioctl(fd, APFS_IOC_TAKE_SNAPSHOT, &arg))
				die("snapshot", fld[2]);
			close(fd);
		} else {
			fprintf(stderr, "apfs_guest: line %ld: bad operation %s\n", lineno, fld[0]);
			return 1;
		}
	}
	free(line);
	fclose(f);
	return 0;
}
