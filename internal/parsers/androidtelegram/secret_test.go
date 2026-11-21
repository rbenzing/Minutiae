package androidtelegram_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/androidtelegram"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// Spec 9 (call 6), 8.6, 4I4-P12 (facts.tsv cast.encrypted_chat_id, DO:117-119): secret-chat messages are flagged and
// keyed by the 0x4000... dialog id. R8 (incoming) is from user:1000002 with raw.secret_chat, dialog_class secret,
// enc_chat_id 77 and the Summary channel "telegram secret"; R9 (outgoing) is to the enc_chats user of chat 77; R10's
// key is the low 32 bits of its uid as the pinned cast reads them (-5 for int32) and its counterparty that row's user.
// A uid with bit 63 set as well (0xC000000000000001) is not secret (the predicate wants bit 63 clear). With enc_chats
// absent, R9's counterparty is undecided: participants unknown, raw.join_undecided ["enc_chats"], the note
// join_undecided.enc_chats "table absent".
func TestTelegramSecretChatMessagesAreFlagged(t *testing.T) {
	rows := byRow(parseFixture(t, "telegram-v181").Records)
	want10 := int64(-5)
	if androidtelegram.EncChatIDCast() != "int32" {
		want10 = 0xFFFFFFFB
	}
	cases := []struct {
		key, role, addr string
		enc             int64
	}{
		{"messages_v2/8", "from", "user:1000002", 77},
		{"messages_v2/9", "to", "user:1000002", 77},
		{"messages_v2/10", "to", "user:1000099", want10},
		{"messages_v2/57", "from", "user:1000002", 77},
	}
	for _, c := range cases {
		r := rows[c.key]
		raw := rawOf(r)
		if raw["secret_chat"] != true || raw["dialog_class"] != "secret" || raw["enc_chat_id"] != c.enc || party(r, c.role) != c.addr {
			t.Errorf("%s: secret_chat %v, dialog_class %v, enc_chat_id %v, %s %q; want %s %q key %d", c.key, raw["secret_chat"],
				raw["dialog_class"], raw["enc_chat_id"], c.role, party(r, c.role), c.role, c.addr, c.enc)
		}
		if th := threadOf(r); th["group"] != false || th["title"] != nil {
			t.Errorf("%s: thread %v, want group false and no title", c.key, th)
		}
		dir := map[string]string{"from": "in", "to": "out"}[c.role]
		if s := r.Summary; !strings.HasPrefix(s, "telegram secret "+dir+" "+c.addr+":") {
			t.Errorf("%s: Summary %q", c.key, s)
		}
	}
	if s := rows["messages_v2/1"].Summary; !strings.HasPrefix(s, "telegram in user:1000002: ") {
		t.Errorf("R1: Summary %q, want the plain channel", s)
	}

	in := tlMessage{id: 1, from: idAnn, peer: idSelf, date: baseDate, text: "high bits"}.bytes()
	got := parseAt(t, remoteApp, dbOnly(goBuiltMessages(181, nil, []msgCells{{mid: 1, uid: -0x3fffffffffffffff, data: in}}))).Records
	if len(got) != 1 || rawOf(got[0])["dialog_class"] == "secret" || rawOf(got[0])["secret_chat"] != nil || rawOf(got[0])["enc_chat_id"] != nil {
		t.Errorf("uid 0xC000000000000001: %v, want not secret", rawOf(got[0]))
	}
	// spec 6.4, 8.6: a negative uid is a basic group, channel or community (the peer is a user here: a conflict)
	if rawOf(got[0])["dialog_class"] != "negative" {
		t.Errorf("uid 0xC000000000000001: dialog_class %v, want negative", rawOf(got[0])["dialog_class"])
	}

	r9 := fixtureBlobs(t, "telegram-v181", "messages_v2", "data")[9]
	res := parseAt(t, remoteApp, dbOnly(goBuiltMessages(181, nil, []msgCells{{mid: -8, uid: 1<<62 | 77, out: int64(1), read: int64(0), data: r9}})))
	if len(res.Records) != 1 {
		t.Fatalf("%d records", len(res.Records))
	}
	r := res.Records[0]
	if r.Payload["participants_unknown"] != true || jsonText(rawOf(r)["join_undecided"]) != `["enc_chats"]` ||
		rawOf(r)["counterparty_state"] != "undecided" || rawOf(r)["secret_chat"] != true {
		t.Errorf("R9 without enc_chats: participants_unknown %v, raw %v", r.Payload["participants_unknown"], rawOf(r))
	}
	if res.Notes["join_undecided.enc_chats"] != "table absent" {
		t.Errorf("note join_undecided.enc_chats = %q", res.Notes["join_undecided.enc_chats"])
	}

	// spec 8.10, 4I4-P9, M4: the note secret_chats counts the distinct secret chats by their enc_chats key, as the
	// oracle does (R8, R9, R57 in chat 77, R10 in chat -5: two in telegram-v181; P2 in telegram-v64); rows whose dialog
	// is undecided (an unreadable uid: four in telegram-hostile, beside its chat 77) make it a lower bound that says so, never a definite 0
	for v, want := range map[string]string{
		"telegram-v181": "2", "telegram-v64": "1", "telegram-v159": "0",
		"telegram-hostile": ">=1 (4 rows with an undecided dialog)",
	} {
		got := parseFixture(t, v).Notes["secret_chats"]
		if exp := parsertestNote(t, v, "secret_chats"); got != want || exp != want {
			t.Errorf("%s: note secret_chats %q, the oracle %q, want %q", v, got, exp, want)
		}
	}
	// one chat in both generations: the pre-84 high-id uid 77<<32 and the v84 dialog id 1<<62|77 share the key 77
	same := tlMessage{id: 1, from: idAnn, peer: idSelf, date: baseDate, text: "same chat"}.bytes()
	both := goBuiltMessages(181, []msgCells{{mid: -7, uid: 77 << 32, data: same}}, []msgCells{{mid: -8, uid: 1<<62 | 77, data: same}})
	if got := parseAt(t, remoteApp, dbOnly(both)).Notes["secret_chats"]; got != "1" {
		t.Errorf("chat 77 in both generations: note secret_chats %q, want 1", got)
	}
}

// encChatsForbidden are the enc_chats columns no parser file may name (spec 9: the v181 DDL's columns but uid and
// user).
func encChatsForbidden() []string {
	return []string{
		"name", "data", "g", "authkey", "ttl", "layer", "seq_in", "seq_out", "use_count", "exchange_id", "key_date",
		"fprint", "fauthkey", "khash", "in_seq_no", "admin_id", "mtproto_seq",
	}
}

// encChatsViolations checks parsed parser files (by base name) for the allowlist of spec 9 (F9): the literal
// "enc_chats" occurs once, as secret.go's const tableEncChats; a call that passes tableEncChats is a Get or joinGet
// (a rowid follows), a Table whose next argument is encChatsColumns(), or a Column whose next argument indexes
// encChatsColumns(); a composite literal with Table: tableEncChats has Need: encChatsColumns(); secret.go holds no
// literal naming another enc_chats column.
func encChatsViolations(fset *token.FileSet, files map[string]*ast.File) []string {
	var out []string
	at := func(n ast.Node, msg string) { out = append(out, fset.Position(n.Pos()).String()+": "+msg) }
	isTable := func(e ast.Expr) bool { id, ok := e.(*ast.Ident); return ok && id.Name == "tableEncChats" }
	isCols := func(e ast.Expr) bool {
		c, ok := e.(*ast.CallExpr)
		if !ok || len(c.Args) != 0 {
			return false
		}
		id, ok := c.Fun.(*ast.Ident)
		return ok && id.Name == "encChatsColumns"
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	slices.Sort(names)
	literal := 0
	allowed := map[*ast.BasicLit]bool{}
	for _, name := range names {
		f := files[name]
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.ValueSpec:
				for i, v := range x.Values {
					if l, ok := v.(*ast.BasicLit); ok && name == "secret.go" && i < len(x.Names) && x.Names[i].Name == "tableEncChats" {
						allowed[l] = true
					}
				}
			case *ast.BasicLit:
				if x.Kind != token.STRING {
					return true
				}
				s, _ := strconv.Unquote(x.Value)
				if s == "enc_chats" {
					if !allowed[x] {
						at(x, `the literal "enc_chats" outside secret.go's const tableEncChats`)
					}
					literal++
				}
				if name == "secret.go" && slices.Contains(encChatsForbidden(), s) {
					at(x, "secret.go names the enc_chats column "+s)
				}
			case *ast.CallExpr:
				for i, a := range x.Args {
					if !isTable(a) {
						continue
					}
					fn := ""
					switch f := x.Fun.(type) {
					case *ast.SelectorExpr:
						fn = f.Sel.Name
					case *ast.Ident:
						fn = f.Name
					}
					next := func() ast.Expr {
						if i+1 < len(x.Args) {
							return x.Args[i+1]
						}
						return nil
					}()
					switch fn {
					case "Get", "joinGet":
					case "Table":
						if next == nil || !isCols(next) {
							at(x, "a Table on tableEncChats without encChatsColumns()")
						}
					case "Column":
						ix, ok := next.(*ast.IndexExpr)
						if !ok || !isCols(ix.X) {
							at(x, "a Column on tableEncChats not named by encChatsColumns()")
						}
					default:
						at(x, fn+" is passed tableEncChats")
					}
				}
			case *ast.CompositeLit:
				table, need := false, false
				for _, e := range x.Elts {
					kv, ok := e.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					k, _ := kv.Key.(*ast.Ident)
					switch {
					case k != nil && k.Name == "Table" && isTable(kv.Value):
						table = true
					case k != nil && k.Name == "Need" && isCols(kv.Value):
						need = true
					}
				}
				if table && !need {
					at(x, "a Req on tableEncChats without Need: encChatsColumns()")
				}
			}
			return true
		})
	}
	if literal != 1 {
		out = append(out, strconv.Itoa(literal)+` literals "enc_chats", want 1`)
	}
	return out
}

// encChatsUses checks every use of the identifier tableEncChats (I3): it may be declared only in secret.go, passed
// as a call argument (the call rules of encChatsViolations apply), be a Req's Table: value, be concatenated in
// secret.go (the unknown_columns key), or be an element of the census table lists of notes.go (rowNoteTables,
// readTables: row counts and unmapped tables, never a column). Any other use (a const or var whose value it is, an
// assignment, a case clause, another list) may alias it past the call rules and is a violation.
func encChatsUses(fset *token.FileSet, files map[string]*ast.File) []string {
	var out []string
	at := func(n ast.Node, msg string) { out = append(out, fset.Position(n.Pos()).String()+": "+msg) }
	for name, f := range files {
		var stack []ast.Node
		ast.Inspect(f, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			if id, ok := n.(*ast.Ident); ok && id.Name == "tableEncChats" && len(stack) > 0 {
				fn := ""
				for _, s := range stack {
					if d, ok := s.(*ast.FuncDecl); ok {
						fn = d.Name.Name
					}
				}
				switch p := stack[len(stack)-1].(type) {
				case *ast.ValueSpec:
					if slices.Contains(p.Names, id) {
						if name != "secret.go" {
							at(id, "tableEncChats declared outside secret.go")
						}
					} else {
						at(id, "a const or var whose value is tableEncChats")
					}
				case *ast.CallExpr:
					if p.Fun == ast.Expr(id) {
						at(id, "tableEncChats called")
					}
				case *ast.KeyValueExpr:
					if k, ok := p.Key.(*ast.Ident); !ok || k.Name != "Table" || p.Value != ast.Expr(id) {
						at(id, "tableEncChats keyed other than as a Req's Table")
					}
				case *ast.BinaryExpr:
					if name != "secret.go" || p.Op != token.ADD {
						at(id, "tableEncChats in an expression outside secret.go")
					}
				case *ast.CompositeLit:
					if name != "notes.go" || (fn != "rowNoteTables" && fn != "readTables") {
						at(id, "tableEncChats in a list that may alias it")
					}
				default:
					at(id, "a use of tableEncChats that may alias it")
				}
			}
			stack = append(stack, n)
			return true
		})
	}
	return out
}

// encChatsPresent is the positive half (I3): the real package holds exactly one Req with Table: tableEncChats and
// Need: encChatsColumns(), and at least one Column call that indexes encChatsColumns() (the user read).
func encChatsPresent(files map[string]*ast.File) []string {
	reqs, cols := 0, 0
	isCols := func(e ast.Expr) bool {
		c, ok := e.(*ast.CallExpr)
		if !ok || len(c.Args) != 0 {
			return false
		}
		id, ok := c.Fun.(*ast.Ident)
		return ok && id.Name == "encChatsColumns"
	}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CompositeLit:
				table, need := false, false
				for _, e := range x.Elts {
					if kv, ok := e.(*ast.KeyValueExpr); ok {
						k, _ := kv.Key.(*ast.Ident)
						v, _ := kv.Value.(*ast.Ident)
						table = table || (k != nil && k.Name == "Table" && v != nil && v.Name == "tableEncChats")
						need = need || (k != nil && k.Name == "Need" && isCols(kv.Value))
					}
				}
				if table && need {
					reqs++
				}
			case *ast.CallExpr:
				if s, ok := x.Fun.(*ast.SelectorExpr); ok && s.Sel.Name == "Column" && len(x.Args) == 2 {
					if ix, ok := x.Args[1].(*ast.IndexExpr); ok && isCols(ix.X) {
						cols++
					}
				}
			}
			return true
		})
	}
	var out []string
	if reqs != 1 {
		out = append(out, strconv.Itoa(reqs)+" Reqs with Table: tableEncChats and Need: encChatsColumns(), want 1")
	}
	if cols == 0 {
		out = append(out, "no Column call indexes encChatsColumns()")
	}
	return out
}

// Spec 9 (call 6), F9, T1, ruling T6 c1: the parser names no enc_chats column but uid and user. encChatsColumns is
// {uid, user} and tableEncChats "enc_chats"; an AST scan of the package's non-test files finds the literal
// "enc_chats" only in secret.go's const, every Table, Req, Column or Get on tableEncChats takes encChatsColumns() or a
// rowid, and secret.go names no other enc_chats column; no other use of tableEncChats may alias it (encChatsUses), and
// the one Req and the user Column read are there (encChatsPresent). A planted file of each wrong form is caught.
func TestTelegramEncChatsColumnsAllowlisted(t *testing.T) {
	if got := androidtelegram.EncChatsColumns(); !slices.Equal(got, []string{"uid", "user"}) || androidtelegram.TableEncChats != "enc_chats" {
		t.Fatalf("encChatsColumns %v, tableEncChats %q", got, androidtelegram.TableEncChats)
	}
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		src, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, p, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		files[filepath.Base(p)] = f
	}
	if files["secret.go"] == nil {
		t.Fatal("no secret.go")
	}
	for _, v := range append(append(encChatsViolations(fset, files), encChatsUses(fset, files)...), encChatsPresent(files)...) {
		t.Error(v)
	}
	if got := strings.Join(encChatsPresent(map[string]*ast.File{"secret.go": mustParse(t, token.NewFileSet(), "secret.go",
		`package p; const tableEncChats = "enc_chats"`)}), "\n"); !strings.Contains(got, "want 1") || !strings.Contains(got, "no Column") {
		t.Errorf("a package without the Req and the Column read: %q", got)
	}

	planted := map[string]string{ //nolint:gosec // G101: planted Go source naming column names, not credentials
		"other.go":   `package p; const x = "enc_chats"`,
		"table.go":   `package p; func f() { db.Table(ctx, tableEncChats, []string{"uid", "g"}, nil) }`,
		"column.go":  `package p; func f() { m.Column(tableEncChats, "authkey") }`,
		"req.go":     `package p; var r = rowrun.Req{Table: tableEncChats, Need: []string{"uid", "fprint"}}`,
		"pass.go":    `package p; func f() { scan(tableEncChats) }`,
		"secret.go":  `package p; const tableEncChats = "enc_chats"; func g() string { return "khash" }`,
		"secret2.go": `package p; const tableEncChats = "enc_chats"`,
		"alias.go":   `package p; const alias = tableEncChats; func f() { db.Table(ctx, alias, []string{"g"}, nil) }`,
		"assign.go":  `package p; func f() { t := tableEncChats; db.Table(ctx, t, []string{"g"}, nil) }`,
		"list.go":    `package p; func f() { for _, t := range []string{tableEncChats} { db.Table(ctx, t, nil, nil) } }`,
		"case.go":    `package p; func f(s string) { switch s { case tableEncChats: } }`,
	}
	wants := map[string]string{ //nolint:gosec // G101: violation messages naming column names, not credentials
		"other.go": `literal "enc_chats" outside`, "table.go": "a Table on tableEncChats", "column.go": "a Column on tableEncChats",
		"req.go": "a Req on tableEncChats", "pass.go": "scan is passed tableEncChats", "secret.go": "names the enc_chats column khash",
		"secret2.go": `literal "enc_chats" outside`, "alias.go": "a const or var whose value is tableEncChats",
		"assign.go": "may alias it", "list.go": "in a list that may alias it", "case.go": "may alias it",
	}
	for name, src := range planted {
		pf := token.NewFileSet()
		set := map[string]*ast.File{"secret.go": mustParse(t, pf, "secret.go", `package p; const tableEncChats = "enc_chats"`)}
		set[name] = mustParse(t, pf, name, src)
		got := strings.Join(append(encChatsViolations(pf, set), encChatsUses(pf, set)...), "\n")
		if !strings.Contains(got, wants[name]) {
			t.Errorf("planted %s: violations %q, want one with %q", name, got, wants[name])
		}
	}
}

// parsertestNote is a note of a variant's oracle.
func parsertestNote(t *testing.T, variant, key string) string {
	t.Helper()
	return parsertest.LoadFixture(t, "testdata", variant).Expect.Notes[key]
}

func mustParse(t *testing.T, fset *token.FileSet, name, src string) *ast.File {
	t.Helper()
	f, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		t.Fatal(err)
	}
	return f
}
