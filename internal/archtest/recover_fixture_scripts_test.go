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

var (
	forHead       = regexp.MustCompile(`^for\s+\w+\s+in\b`)
	doWord        = regexp.MustCompile(`\bdo\b`)
	dataArg       = regexp.MustCompile(`\b(grep|egrep|fgrep|awk)((?:\s+-[A-Za-z]+)*)\s+('[^']*'|"[^"]*")`)
	shellFromData = regexp.MustCompile(`^(sh|bash|dash|zsh|ash)\s*$|\b(sh|bash|dash|zsh|ash)\s+(-\w+\s+)*-\w*c\b|\beval\b`)
)

// fileWaivers are the constructs an older script uses by documented design,
// named one by one and removed from its segments before the rule is applied:
// anything else forbidden in the same file still fails. Each entry points at
// the README text that approved it.
//   - exfat.sh: README.md "exFAT needs a privileged container" (exfat-fuse on a loop device).
//   - f2fs.sh: only the words "cannot mount f2fs" inside the generator.note text; the script never mounts (ruling F37).
//   - recover_probe.sh: README.md "Recovery fixtures: toolchain probe" (queries the installed exfat-fuse package).
var fileWaivers = map[string][]*regexp.Regexp{
	"exfat.sh": {
		regexp.MustCompile(`/dev/fuse`), regexp.MustCompile(`\blosetup\b`), regexp.MustCompile(`mount\.exfat-fuse`),
		regexp.MustCompile(`exfat-fuse`), regexp.MustCompile(`--privileged`), regexp.MustCompile(`did not mount`),
	},
	"f2fs.sh": {
		regexp.MustCompile(`cannot mount f2fs`), // words inside the generator.note text only
	},
	"recover_probe.sh": {regexp.MustCompile(`exfat-fuse`)},
}

// guardViolations returns what the module rule objects to in one script: every
// host command segment (comments stripped) is checked on its own. Only the word
// list after "for x in", and the quoted arguments of grep/awk, count as data.
func guardViolations(name, src string) []string {
	var out []string
	host, _ := splitGuest(src)
	var clean []string
	for _, line := range strings.Split(host, "\n") {
		clean = append(clean, stripComment(line))
	}
	text := dataArg.ReplaceAllString(strings.Join(clean, "\n"), "$1$2 ''")
	for _, seg := range segmentSplit.Split(text, -1) {
		seg = strings.TrimSpace(seg)
		switch {
		case forHead.MatchString(seg):
			if loc := doWord.FindStringIndex(seg); loc != nil {
				seg = seg[loc[0]:] // the word list ends at "do"; what follows is a command
			} else {
				seg = ""
			}
		}
		for _, w := range fileWaivers[name] {
			seg = w.ReplaceAllString(seg, " ")
		}
		if seg == "" {
			continue
		}
		if m := hostForbidden.FindString(seg); m != "" {
			out = append(out, "host kernel route "+m+": "+seg)
		}
		if m := fuseWords.FindString(seg); m != "" {
			out = append(out, "host kernel route "+m+": "+seg)
		}
		if m := shellFromData.FindString(seg); m != "" {
			out = append(out, "shell fed from text or eval ("+m+"): "+seg)
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
		file string
		src  string
		bad  bool
	}{
		{"clean init heredoc", "x.sh", initHere + "modprobe -S k -d / --show-depends vfat | awk '{print}'\n", false},
		{"comment may name the words", "x.sh", "# no insmod, no --privileged, no losetup here\necho hi\n", false},
		{"insmod in another heredoc", "x.sh", initHere + "bash <<'EOT'\ninsmod x\nEOT\n", true},
		{"insmod in an unquoted-tag heredoc", "x.sh", "sh <<EOF\ninsmod x\nEOF\n", true},
		{"cap-add", "x.sh", "docker run --cap-add SYS_ADMIN x\n", true},
		{"device", "x.sh", "docker run --device /dev/loop0 x\n", true},
		{"mount without -t", "x.sh", "mount /dev/loop0 /mnt\n", true},
		{"mixed modprobe segment", "x.sh", "modprobe --show-depends a; modprobe a\n", true},
		{"mixed modprobe with &&", "x.sh", "modprobe -S k --show-depends a && modprobe a\n", true},
		{"modprobe remove", "x.sh", "modprobe -r vfat\n", true},
		{"privileged", "x.sh", "docker run --privileged x\n", true},
		{"fuse", "x.sh", "ls /dev/fuse\n", true},
		{"losetup", "x.sh", "x=$(losetup -f)\n", true},
		{"trailing comment is not code", "x.sh", "echo hi # insmod later\n", false},
		{"pure listing", "x.sh", "deps=$(modprobe -S k -d / --show-depends m 2>/dev/null)\n", false},
		{"echo piped to sh", "x.sh", "echo insmod x | sh\n", true},
		{"eval", "x.sh", "eval \"modprobe x\"\n", true},
		{"bash -c", "x.sh", "bash -c 'mount /dev/x /mnt'\n", true},
		{"for body insmod", "x.sh", "for m in a; do insmod $m; done\n", true},
		{"for body insmod one line", "x.sh", "for m in a do insmod $m; done\n", true},
		{"for word list is data", "x.sh", "for a in sh mount insmod; do echo $a; done\n", false},
		{"echo naming a word is not exempt", "x.sh", "echo insmod x\n", true},
		{"dpkg naming exfat-fuse is not exempt", "x.sh", "dpkg -s exfat-fuse\n", true},
		{"grep pattern is data", "x.sh", "grep -Eq '^(insmod|builtin) ' f\n", false},
		{"awk pattern is data", "x.sh", "awk '$1==\"insmod\"{print $2}' f\n", false},
		{"grep then insmod", "x.sh", "grep -q a f && insmod x\n", true},
		{"exfat.sh may use a loop device", "exfat.sh", "losetup -f\n", false},
		{"exfat.sh plus insmod fails", "exfat.sh", "losetup -f\ninsmod x\n", true},
		{"exfat.sh plus mount -t fails", "exfat.sh", "mount -t vfat a b\n", true},
		{"other script may not use losetup", "x.sh", "losetup -f\n", true},
		{"f2fs.sh plus a kernel mount fails", "f2fs.sh", "mount -t f2fs -o loop \"$img\" \"$mnt\"\n", true},
		{"f2fs.sh plus insmod fails", "f2fs.sh", "insmod x\n", true},
		{"f2fs.sh note naming the words stays allowed", "f2fs.sh", "note=\"the kernel cannot mount f2fs (no module)\"\n", false},
	}
	for _, c := range cases {
		if got := guardViolations(c.file, c.src); (len(got) > 0) != c.bad {
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
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		for _, v := range guardViolations(filepath.Base(f), string(b)) {
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
