package archtest

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The guard below is a regression TRIPWIRE against accidental host-side kernel
// or privilege use in OUR fixture scripts. It is not a sandbox: a deliberately
// obfuscated script (string-built command names such as ins""mod or ${m}mod,
// indirection through files it writes itself) is out of scope, and reviewers
// still read every fixture script (ruling F39 h).

var (
	// the one heredoc that becomes the guest's /init: `cat >"$rd/init" <<'EOT'` at the start of a command
	guestInitPrefix = regexp.MustCompile(`^\s*cat\s*>\s*"?\$\{?rd\}?/init"?\s*$`)
	segmentSplit    = regexp.MustCompile(`;|&&|\|\||\||&|\n`)
	forHead         = regexp.MustCompile(`^for\s+\w+\s+in\b`)
	doWord          = regexp.MustCompile(`\bdo\b`)
	// a single-quoted grep/awk argument is data (a pattern or program), unless it can run something
	dataArg = regexp.MustCompile(`\b(grep|egrep|fgrep|awk)((?:\s+-[A-Za-z]+)*)\s+('[^']*')`)
	// forbidden in any host segment: the host and Docker VM kernel must never be touched
	hostForbidden = regexp.MustCompile(`--privileged|--cap-add|--device|--security-opt|--pid=host|/dev/fuse|\blosetup\b|\binsmod\b|\brmmod\b|\bdepmod\b|\bmknod\b|\bmount\b|\bnsenter\b|\bsystem\s*\(`)
	fuseWords     = regexp.MustCompile(`exfat-fuse|mount\.exfat`)
	modprobeWord  = regexp.MustCompile(`\bmodprobe\b`)
	shellWords    = regexp.MustCompile(`^(sh|bash|dash|zsh|ash)$`)
	shellFromData = regexp.MustCompile(`^(sh|bash|dash|zsh|ash)\s*$|\b(sh|bash|dash|zsh|ash)\s+(-\w+\s+)*-\w*c\b|\beval\b`)
	// sourcing or running a script is allowed only for a file of tools/fixtures
	allowedScript = regexp.MustCompile(`^"?(\$\{?here\}?|tools/fixtures)/([A-Za-z0-9_.-]|\$\{?\w+\}?)+\.sh"?$`)
	initPath      = regexp.MustCompile(`(^|/)init"?$`)
	assignWord    = regexp.MustCompile(`^[A-Za-z_]\w*=`)
	operatorStart = regexp.MustCompile(`^[-+*=|{\[(<>!0-9]`)
)

func isSep(c rune) bool {
	return c == ' ' || c == '\t' || c == ';' || c == '&' || c == '|' || c == '('
}

// splitGuest returns the host text (comments dropped, every heredoc body other
// than the guest /init one included) and the text of the guest /init heredoc.
// Quotes, escapes and comments are honoured, so an opener inside a comment or a
// quoted string is not a heredoc, and a backslash-newline joins lines.
func splitGuest(src string) (host, guest string) {
	src = strings.ReplaceAll(src, "\\\n", "")
	var h, g strings.Builder
	sq, dq := false, false
	tag, tagGuest := "", false
	for _, line := range strings.Split(src, "\n") {
		if tag != "" {
			switch {
			case strings.TrimSpace(line) == tag:
				tag = ""
			case tagGuest:
				g.WriteString(line + "\n")
			case !strings.HasPrefix(strings.TrimSpace(line), "#"):
				h.WriteString(line + "\n")
			}
			continue
		}
		var out strings.Builder
		rs := []rune(line)
		wordStart := true
		pending := ""
		for i := 0; i < len(rs); i++ {
			c := rs[i]
			if sq {
				out.WriteRune(c)
				sq = c != '\''
				continue
			}
			if dq {
				out.WriteRune(c)
				if c == '\\' && i+1 < len(rs) {
					i++
					out.WriteRune(rs[i])
				} else if c == '"' {
					dq = false
				}
				continue
			}
			switch {
			case c == '\\':
				out.WriteRune(c)
				if i+1 < len(rs) {
					i++
					out.WriteRune(rs[i])
				}
				wordStart = false
				continue
			case c == '\'':
				sq, wordStart = true, false
			case c == '"':
				dq, wordStart = true, false
			case c == '#' && wordStart:
				i = len(rs) // a comment: the rest of the line is not code
				continue
			case c == '<' && i+1 < len(rs) && rs[i+1] == '<' && (i == 0 || rs[i-1] != '<') && (i+2 >= len(rs) || rs[i+2] != '<'):
				j := i + 2
				if j < len(rs) && rs[j] == '-' {
					j++
				}
				for j < len(rs) && (rs[j] == ' ' || rs[j] == '\t') {
					j++
				}
				k := j
				name := ""
				if k < len(rs) && (rs[k] == '\'' || rs[k] == '"') {
					q := rs[k]
					k++
					for k < len(rs) && rs[k] != q {
						name += string(rs[k])
						k++
					}
					k++
				} else {
					for k < len(rs) && (rs[k] == '_' || rs[k] >= '0' && rs[k] <= '9' || rs[k] >= 'A' && rs[k] <= 'Z' || rs[k] >= 'a' && rs[k] <= 'z') {
						name += string(rs[k])
						k++
					}
				}
				if name != "" {
					tagGuest = guestInitPrefix.MatchString(out.String())
					pending = name
					out.WriteString(string(rs[i:min(k, len(rs))]))
					i = k - 1
					continue
				}
			default:
				wordStart = isSep(c)
			}
			out.WriteRune(c)
			if c != '\'' && c != '"' {
				wordStart = isSep(c)
			}
		}
		h.WriteString(out.String() + "\n")
		if pending != "" {
			tag = pending
		}
	}
	return h.String(), g.String()
}

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
	// the images install the exfat-fuse PACKAGE for exfat.sh (README.md "exFAT needs a privileged container"); nothing is mounted at build time
	"Dockerfile":         {regexp.MustCompile(`exfat-fuse`)},
	"Dockerfile.recover": {regexp.MustCompile(`exfat-fuse`)},
}

// modprobeIsListing reports whether a segment that names modprobe is a pure
// dependency listing: --show-depends present, every other flag exactly -S or -d
// with its value (whole tokens, no bundled flags).
func modprobeIsListing(seg string) bool {
	loc := modprobeWord.FindStringIndex(seg)
	if loc == nil {
		return true
	}
	toks := strings.Fields(seg[loc[1]:])
	listing := false
	for i := 0; i < len(toks); i++ {
		t := strings.Trim(toks[i], `"'()`+"`")
		switch {
		case t == "--show-depends":
			listing = true
		case t == "-S" || t == "-d":
			i++ // its value
		case strings.HasPrefix(t, "-"):
			return false
		case strings.Contains(t, "--show-depends"):
			return false // a substring of some other word is not the flag
		}
	}
	return listing
}

// scriptOperand returns the file a segment sources or runs, if any.
func scriptOperand(words []string) (cmd, op string) {
	for len(words) > 0 && (assignWord.MatchString(words[0]) && !strings.Contains(words[0], "$(") || words[0] == "{" || words[0] == "!" || words[0] == "then" || words[0] == "do" || words[0] == "if") {
		words = words[1:]
	}
	if len(words) == 0 {
		return "", ""
	}
	cmd = strings.Trim(words[0], `"`)
	rest := words[1:]
	switch {
	case cmd == "." || cmd == "source" || shellWords.MatchString(cmd):
		for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
			rest = rest[1:]
		}
		if len(rest) > 0 {
			return cmd, rest[0]
		}
		return cmd, ""
	case strings.Contains(cmd, "/") && strings.HasSuffix(cmd, ".sh"):
		return cmd, words[0]
	}
	return cmd, ""
}

// segmentViolations applies the host rules to one command segment.
func segmentViolations(seg string) []string {
	var out []string
	if m := hostForbidden.FindString(seg); m != "" {
		out = append(out, "host kernel route "+m+": "+seg)
	}
	if m := fuseWords.FindString(seg); m != "" {
		out = append(out, "host kernel route "+m+": "+seg)
	}
	if m := shellFromData.FindString(seg); m != "" {
		out = append(out, "shell fed from text or eval ("+m+"): "+seg)
	}
	if modprobeWord.MatchString(seg) && !modprobeIsListing(seg) {
		out = append(out, "modprobe that is not a pure listing: "+seg)
	}
	words := strings.Fields(seg)
	cmd, op := scriptOperand(words)
	if cmd == "." && operatorStart.MatchString(op) {
		cmd, op = "", "" // a jq/awk expression such as ". + {...}", not the source builtin
	}
	if op != "" && !allowedScript.MatchString(op) && (cmd == "." || cmd == "source" || shellWords.MatchString(cmd) || strings.Contains(cmd, "/")) {
		out = append(out, "sources or runs a script outside tools/fixtures: "+seg)
	}
	if initPath.MatchString(strings.Trim(cmd, `"`)) || initPath.MatchString(strings.Trim(op, `"`)) {
		out = append(out, "runs the guest init on the host: "+seg)
	}
	if cmd == "chroot" || cmd == "exec" || cmd == "env" || cmd == "nohup" || cmd == "timeout" {
		for _, w := range words[1:] {
			if initPath.MatchString(strings.Trim(w, `"`)) {
				out = append(out, "runs the guest init on the host: "+seg)
			}
		}
	}
	return out
}

// guardViolations returns what the module rule objects to in one shell script:
// every host command segment (comments stripped) is checked on its own. Only
// the word list after "for x in" (when it holds no command substitution) and
// single-quoted grep/awk arguments count as data.
func guardViolations(name, src string) []string {
	host, _ := splitGuest(src)
	text := dataArg.ReplaceAllStringFunc(host, func(m string) string {
		if strings.Contains(m, "system(") || strings.Contains(m, "getline") {
			return m
		}
		return dataArg.ReplaceAllString(m, "$1$2 ''")
	})
	var out []string
	for _, seg := range segmentSplit.Split(text, -1) {
		seg = strings.TrimSpace(seg)
		if forHead.MatchString(seg) && !strings.Contains(seg, "$(") && !strings.Contains(seg, "`") {
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
		out = append(out, segmentViolations(seg)...)
	}
	return out
}

// dockerViolations applies the host rules to the RUN instructions of a
// Dockerfile: a build-time module command would load into the Docker VM.
func dockerViolations(name, src string) []string {
	src = strings.ReplaceAll(src, "\\\n", "")
	var run []string
	for _, line := range strings.Split(src, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "RUN ") {
			run = append(run, strings.TrimPrefix(t, "RUN "))
		}
	}
	return guardViolations(name, strings.Join(run, "\n"))
}

var pyCall = regexp.MustCompile(`(subprocess\.\w+|os\.system|os\.popen|\bPopen|check_output|check_call)\s*\(`)

// pyViolations applies the host rules to the arguments of every subprocess or
// os.system style call (balanced parentheses, so multi-line calls are covered).
func pyViolations(name, src string) []string {
	var out []string
	for _, loc := range pyCall.FindAllStringIndex(src, -1) {
		depth, end := 0, len(src)
		for i := loc[1] - 1; i < len(src); i++ {
			if src[i] == '(' {
				depth++
			} else if src[i] == ')' {
				depth--
				if depth == 0 {
					end = i
					break
				}
			}
		}
		out = append(out, guardViolations(name, src[loc[1]:end])...)
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
		// ruling F39 (a): an opener in a comment or a quoted string is not a heredoc
		{"opener in a comment", "x.sh", "# cat > $rd/init <<EOT\ninsmod x\nEOT\n", true},
		{"opener in a quoted string", "x.sh", "echo \"> $rd/init <<EOT\"\ninsmod x\nEOT\n", true},
		{"real init heredoc still guest", "x.sh", "cat >\"$rd/init\" <<'EOT'\ninsmod x\nEOT\n", false},
		// (b): command substitutions are checked wherever they appear
		{"for list substitution", "x.sh", "for x in $(modprobe y); do :; done\n", true},
		{"for list backtick", "x.sh", "for x in `modprobe y`; do :; done\n", true},
		{"grep double-quoted substitution", "x.sh", "grep \"$(insmod x)\" f\n", true},
		{"awk system", "x.sh", "awk 'BEGIN{system(\"sh x\")}'\n", true},
		// (c): sourcing or running scripts only from tools/fixtures
		{"source from here", "x.sh", ". \"$here/lib.sh\"\n", false},
		{"source parent", "x.sh", ". ../other.sh\n", true},
		{"source tmp", "x.sh", "source /tmp/evil.sh\n", true},
		{"bash tmp script", "x.sh", "bash /tmp/e.sh\n", true},
		{"source variable", "x.sh", ". \"$s\"\n", true},
		{"run local script", "x.sh", "./other.sh\n", true},
		// (d): the guest init never runs on the host
		{"run init with sh", "x.sh", "sh \"$rd/init\"\n", true},
		{"run init directly", "x.sh", "\"$rd/init\"\n", true},
		{"chmod init is fine", "x.sh", "chmod +x \"$rd/init\"\n", false},
		// (e), (8): continuations and escaped spaces
		{"backslash continuation", "x.sh", "ins\\\nmod x\n", true},
		{"escaped space before hash", "x.sh", "echo a\\ #b; insmod x\n", true},
		// (f): modprobe flags are matched as whole tokens
		{"bundled modprobe flags", "x.sh", "modprobe -vr x --show-depends\n", true},
		{"substring show-depends", "x.sh", "modprobe x /tmp/--show-depends\n", true},
		{"listing with -d and -S", "x.sh", "modprobe -S 6.1 -d / --show-depends vfat\n", false},
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
	// ruling F39 (g): the Dockerfiles (build-time commands run in the Docker VM) and the Python helpers
	for pattern, check := range map[string]func(string, string) []string{"Dockerfile*": dockerViolations, "*.py": pyViolations} {
		others, err := filepath.Glob(filepath.Join(dir, pattern))
		if err != nil || len(others) == 0 {
			t.Fatalf("no %s files found: %v", pattern, err)
		}
		for _, f := range others {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Errorf("%s: %v", f, err)
				continue
			}
			for _, v := range check(filepath.Base(f), string(b)) {
				t.Errorf("%s: %s", filepath.Base(f), v)
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

func TestRecoverFixtureGuardDockerAndPython(t *testing.T) {
	cases := []struct {
		name string
		run  func(string, string) []string
		src  string
		bad  bool
	}{
		{"dockerfile modprobe", dockerViolations, "FROM x\nRUN modprobe x\n", true},
		{"dockerfile continued insmod", dockerViolations, "FROM x\nRUN apt-get update \\n && insmod y\n", true},
		{"dockerfile install is fine", dockerViolations, "FROM x\nRUN apt-get install -y kmod\n", false},
		{"dockerfile comment is fine", dockerViolations, "# RUN modprobe x\nFROM x\n", false},
		{"python subprocess insmod", pyViolations, "import subprocess\nsubprocess.run([\"insmod\", \"x\"])\n", true},
		{"python os.system mount", pyViolations, "import os\nos.system(\"mount /dev/x /mnt\")\n", true},
		{"python multi-line check_output", pyViolations, "subprocess.check_output(\n    [\"modprobe\", \"x\"],\n)\n", true},
		{"python fixed tool call", pyViolations, "subprocess.run([\"dump.exfat\", \"-V\"])\n", false},
		{"python text naming the word", pyViolations, "print(\"insmod\")\n", false},
	}
	for _, c := range cases {
		if got := c.run("x", c.src); (len(got) > 0) != c.bad {
			t.Errorf("%s: violations %q, want bad=%v", c.name, got, c.bad)
		}
	}
}
