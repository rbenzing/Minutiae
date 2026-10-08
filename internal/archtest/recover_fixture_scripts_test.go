package archtest

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	heredocOpen = regexp.MustCompile(`<<-?\s*['"]?([A-Za-z_][A-Za-z0-9_]*)['"]?`)
	// the one heredoc that becomes the guest's /init: its redirect target is $rd/init
	guestInitOpen = regexp.MustCompile(`>\s*"?\$\{?rd\}?/init"?\s*<<`)
	segmentSplit  = regexp.MustCompile(`;|&&|\|\||\||&|\n`)
	trailingCmt   = regexp.MustCompile(`\s#.*$`)
	// forbidden in any host segment: the host and Docker VM kernel must never be touched
	hostForbidden  = regexp.MustCompile(`--privileged|--cap-add|--device|--security-opt|--pid=host|/dev/fuse|\blosetup\b|\binsmod\b|\brmmod\b|\bdepmod\b|\bmknod\b|\bmount\b|\bnsenter\b`)
	fuseWords      = regexp.MustCompile(`exfat-fuse|mount\.exfat`)
	modprobeWord   = regexp.MustCompile(`\bmodprobe\b`)
	modprobeRemove = regexp.MustCompile(`\s-r\b|--remove`)
	listingOnly    = regexp.MustCompile(`^(awk|grep|echo|printf|dpkg|for|case)\b`)
)

// splitGuest returns the script text outside heredocs plus the text of every
// heredoc other than the guest /init one (both are host text), and the guest
// /init heredoc text. Only a heredoc redirected to "$rd/init" is guest text.
func splitGuest(src string) (host, guest string) {
	var h, g strings.Builder
	tag, inGuest := "", false
	for _, line := range strings.Split(src, "\n") {
		switch {
		case tag != "":
			if strings.TrimSpace(line) == tag {
				tag, inGuest = "", false
				continue
			}
			if inGuest {
				g.WriteString(line + "\n")
			} else {
				h.WriteString(line + "\n")
			}
		default:
			h.WriteString(line + "\n")
			if m := heredocOpen.FindStringSubmatch(line); m != nil {
				tag, inGuest = m[1], guestInitOpen.MatchString(line)
			}
		}
	}
	return h.String(), g.String()
}

func stripComment(line string) string {
	if strings.HasPrefix(strings.TrimSpace(line), "#") {
		return ""
	}
	if loc := trailingCmt.FindStringIndex(line); loc != nil && strings.Count(line[:loc[0]], `"`)%2 == 0 && strings.Count(line[:loc[0]], "'")%2 == 0 {
		return line[:loc[0]]
	}
	return line
}

// guardViolations returns what the module rule objects to in one script: every
// host command segment (comments stripped) is checked on its own.
func guardViolations(src string) []string {
	var out []string
	host, _ := splitGuest(src)
	var clean []string
	for _, line := range strings.Split(host, "\n") {
		clean = append(clean, stripComment(line))
	}
	for _, seg := range segmentSplit.Split(strings.Join(clean, "\n"), -1) {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		if listingOnly.MatchString(seg) {
			continue // a name inside an awk/grep/echo/for list is text, not a command
		}
		if m := hostForbidden.FindString(seg); m != "" {
			out = append(out, "host kernel route "+m+": "+seg)
		}
		if m := fuseWords.FindString(seg); m != "" && !strings.HasPrefix(seg, "dpkg ") && !strings.HasPrefix(seg, "echo ") {
			out = append(out, "host kernel route "+m+": "+seg)
		}
		if modprobeWord.MatchString(seg) && (!strings.Contains(seg, "--show-depends") || modprobeRemove.MatchString(seg)) {
			out = append(out, "modprobe that is not a pure listing: "+seg)
		}
	}
	return out
}

func TestRecoverFixtureGuardTable(t *testing.T) {
	initHere := "cat >\"$rd/init\" <<'EOT'\ninsmod m\nmount -t vfat /dev/vda /mnt\nEOT\n"
	cases := []struct {
		name string
		src  string
		bad  bool
	}{
		{"clean init heredoc", initHere + "modprobe -S k -d / --show-depends vfat | awk '{print}'\n", false},
		{"comment may name the words", "# no insmod, no --privileged, no losetup here\necho hi\n", false},
		{"insmod in another heredoc", initHere + "bash <<'EOT'\ninsmod x\nEOT\n", true},
		{"insmod in an unquoted-tag heredoc", "sh <<EOF\ninsmod x\nEOF\n", true},
		{"cap-add", "docker run --cap-add SYS_ADMIN x\n", true},
		{"device", "docker run --device /dev/loop0 x\n", true},
		{"mount without -t", "mount /dev/loop0 /mnt\n", true},
		{"mixed modprobe segment", "modprobe --show-depends a; modprobe a\n", true},
		{"mixed modprobe with &&", "modprobe -S k --show-depends a && modprobe a\n", true},
		{"modprobe remove", "modprobe -r vfat\n", true},
		{"privileged", "docker run --privileged x\n", true},
		{"fuse", "ls /dev/fuse\n", true},
		{"losetup", "x=$(losetup -f)\n", true},
		{"trailing comment is not code", "echo hi # insmod later\n", false},
		{"pure listing", "deps=$(modprobe -S k -d / --show-depends m 2>/dev/null)\n", false},
	}
	for _, c := range cases {
		if got := guardViolations(c.src); (len(got) > 0) != c.bad {
			t.Errorf("%s: violations %q, want bad=%v", c.name, got, c.bad)
		}
	}
}

func TestRecoverFixtureScriptsLoadNoKernelModules(t *testing.T) {
	dir := filepath.Join(repoRoot(t), "tools", "fixtures")
	// the named scripts must exist: the rule cannot silently cover nothing
	for _, name := range []string{"recover_guest.sh", "recover_fat_probe.sh", "fat_recover.sh", "exfat_recover.sh"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.sh"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixture scripts found: %v", err)
	}
	// scripts written before the rule that use a loop device, FUSE or a host mount by documented design
	// (README.md, "exFAT"/"F2FS"); every other script, new ones included, is held to the full rule
	legacy := map[string]bool{"exfat.sh": true, "f2fs.sh": true}
	for _, f := range files {
		if legacy[filepath.Base(f)] {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		for _, v := range guardViolations(string(b)) {
			t.Errorf("%s: %s", filepath.Base(f), v)
		}
		if filepath.Base(f) == "recover_guest.sh" {
			if _, guest := splitGuest(string(b)); !strings.Contains(guest, "insmod") {
				t.Errorf("recover_guest.sh: the guest init must load its modules itself") // the splitter cannot go blind
			}
		}
	}
}

func TestSplitGuestSeparatesHeredocs(t *testing.T) {
	src := "echo a\ncat >\"$rd/init\" <<'EOT'\ninsmod m\nEOT\nmodprobe b\n"
	host, guest := splitGuest(src)
	if strings.Contains(host, "insmod") || !strings.Contains(guest, "insmod m") || !strings.Contains(host, "modprobe b") {
		t.Fatalf("host %q guest %q", host, guest)
	}
}
