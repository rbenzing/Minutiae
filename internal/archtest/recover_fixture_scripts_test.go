package archtest

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	// outside a guest heredoc: no module command (modprobe --show-depends only lists dependencies)
	moduleWords = regexp.MustCompile(`\b(modprobe|insmod|rmmod|depmod)\b`)
	// anywhere: no route that touches the host or Docker VM kernel
	hostKernelWords = regexp.MustCompile(`exfat-fuse|mount\.exfat|/dev/fuse|--privileged|\blosetup\b|\bmknod\b`)
	hostMount       = regexp.MustCompile(`(^|[;&|]\s*|\s)mount\s+-t\s`)
)

// splitGuest returns the script text outside <<'EOT' ... EOT heredocs and the text inside them.
func splitGuest(src string) (host, guest string) {
	var h, g strings.Builder
	in := false
	for _, line := range strings.Split(src, "\n") {
		switch {
		case !in && strings.Contains(line, "<<'EOT'"):
			in = true
			h.WriteString(line + "\n")
		case in && strings.TrimSpace(line) == "EOT":
			in = false
		case in:
			g.WriteString(line + "\n")
		default:
			h.WriteString(line + "\n")
		}
	}
	return h.String(), g.String()
}

func TestRecoverFixtureScriptsLoadNoKernelModules(t *testing.T) {
	for _, name := range []string{"recover_guest.sh", "recover_fat_probe.sh", "fat_recover.sh", "exfat_recover.sh"} {
		b, err := os.ReadFile(filepath.Join(repoRoot(t), "tools", "fixtures", name))
		if err != nil {
			t.Errorf("%s: %v", name, err) // a missing script fails: the rule cannot silently cover nothing
			continue
		}
		host, guest := splitGuest(string(b))
		for _, line := range strings.Split(host, "\n") {
			if moduleWords.MatchString(line) && !strings.Contains(line, "--show-depends") {
				t.Errorf("%s: module command outside a guest heredoc: %q", name, line)
			}
			if hostMount.MatchString(line) {
				t.Errorf("%s: host-side mount: %q", name, line)
			}
		}
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue // a comment may say what the script does not do
			}
			if loc := hostKernelWords.FindString(line); loc != "" {
				t.Errorf("%s uses %q: fixtures of this plan run in the QEMU guest only: %q", name, loc, line)
			}
		}
		if name == "recover_guest.sh" && !strings.Contains(guest, "insmod") {
			t.Errorf("%s: the guest init must load its modules itself (inside the guest)", name) // the splitter cannot go blind
		}
	}
}

// The splitter is proven on text, so the rule cannot silently stop seeing a heredoc.
func TestSplitGuestSeparatesHeredocs(t *testing.T) {
	src := "echo a\ncat >x <<'EOT'\ninsmod m\nEOT\nmodprobe b\n"
	host, guest := splitGuest(src)
	if strings.Contains(host, "insmod") || !strings.Contains(guest, "insmod m") || !strings.Contains(host, "modprobe b") {
		t.Fatalf("host %q guest %q", host, guest)
	}
}
