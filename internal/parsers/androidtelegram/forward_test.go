package androidtelegram_test

import (
	"testing"
)

// The forward header constructors of the Go-built rows (constructors.tsv).
const (
	idFwdHeader    = 0x4e4df4bb // TL_messageFwdHeader: flags, from_id?(0) Peer, from_name?(5), date, channel_post?(2), post_author?(3), ...
	idFwdLayer68   = 0xc786ddcb // TL_messageFwdHeader_layer68: flags, from_id?(0) id32@user, date, from_id?(1) id32@channel, channel_post?(2)
	idPeerChannelB = 0xa2a5371e // TL_peerChannel
)

func peerChannel(id int64) []byte { return append(le32(idPeerChannelB), le64(id)...) }

// fwdModern is a TL_messageFwdHeader with flags, the from peer (when given), from_name (when given), date and
// post_author (when given).
func fwdModern(from []byte, fromName string, date int32, postAuthor string) []byte {
	var flags uint32
	if from != nil {
		flags |= 1
	}
	if fromName != "" {
		flags |= 1 << 5
	}
	if postAuthor != "" {
		flags |= 1 << 3
	}
	b := append(le32(idFwdHeader), le32(flags)...)
	b = append(b, from...)
	if fromName != "" {
		b = append(b, tlStr(fromName)...)
	}
	b = append(b, le32(uint32(date))...)
	if postAuthor != "" {
		b = append(b, tlStr(postAuthor)...)
	}
	return b
}

// fwdLayer68 is a TL_messageFwdHeader_layer68 with the user id (bit 0) and the channel id (bit 1) when non-zero.
func fwdLayer68(user, channel int32, date int32) []byte {
	var flags uint32
	if user != 0 {
		flags |= 1
	}
	if channel != 0 {
		flags |= 2
	}
	b := append(le32(idFwdLayer68), le32(flags)...)
	if user != 0 {
		b = append(b, le32(uint32(user))...)
	}
	b = append(b, le32(uint32(date))...)
	if channel != 0 {
		b = append(b, le32(uint32(channel))...)
	}
	return b
}

// Spec 8.5 raw list (raw.fwd as decoded, peers as 8.6 addresses), 4I4-P11 and T5 (an inline id's class is the wire
// field's meaning in its layer, pinned in the table; never a guess): R3 and R46 as the facts; the seven
// telegram-layers forward rows (1101-1107, one per MessageFwdHeader constructor, written with the date only) state
// the date and no origin; Go-built headers state a Peer origin by its class, an old layer's inline ids by their
// pinned class, from_name and post_author cleaned, and two inline origins in one header as a conflict (both kept,
// none chosen).
func TestTelegramForwardHeaders(t *testing.T) {
	rows := byRow(parseFixture(t, "telegram-v181").Records)
	for id, want := range map[int64]string{
		3:  `{"date":1699999940,"from":"user:1000003"}`,
		46: `{"date":1699999900,"from":"user:1000002"}`,
	} {
		if got := jsonText(rawOf(rows["messages_v2/"+itoa(id)])["fwd"]); got != want {
			t.Errorf("R%d: raw.fwd %s, want %s", id, got, want)
		}
	}
	if f := rawOf(rows["messages_v2/1"])["fwd"]; f != nil {
		t.Errorf("R1: raw.fwd %v: no forward header, no fwd", f)
	}
	layers := byRow(parseFixture(t, "telegram-layers").Records)
	for id := int64(1101); id <= 1107; id++ {
		if got := jsonText(rawOf(layers["messages_v2/"+itoa(id)])["fwd"]); got != `{"date":1700000000}` {
			t.Errorf("layers %d: raw.fwd %s, want the date only", id, got)
		}
	}

	const ann, self = 1000002, 1000001
	msg := func(id int32, fwd []byte) []byte {
		return tlMessage{id: id, from: ann, peer: self, date: baseDate, text: "f", fwd: fwd}.bytes()
	}
	cases := []struct {
		name string
		fwd  []byte
		want string
	}{
		{"channel peer", fwdModern(peerChannel(3000001), "", baseDate-5, "Ed"), `{"date":1699999995,"from":"channel:3000001","post_author":"Ed"}`},
		{"hidden name", fwdModern(nil, "Hidden \x00Name", baseDate-6, ""), `{"date":1699999994,"from_name":"Hidden �Name"}`},
		{"layer68 user", fwdLayer68(1000003, 0, baseDate-7), `{"date":1699999993,"from":"user:1000003"}`},
		{"layer68 channel", fwdLayer68(0, 3000002, baseDate-8), `{"date":1699999992,"from":"channel:3000002"}`},
		{"layer68 both", fwdLayer68(1000003, 3000002, baseDate-9), `{"date":1699999991,"from_ids":"user:1000003,channel:3000002","from_state":"conflict"}`},
	}
	var cells []msgCells
	for i, c := range cases {
		cells = append(cells, msgCells{mid: int64(i + 1), uid: ann, data: msg(int32(i+1), c.fwd)})
	}
	got := byRow(parseAt(t, remoteApp, dbOnly(goBuiltMessages(181, nil, cells))).Records)
	for i, c := range cases {
		raw := rawOf(got["messages_v2/"+itoa(int64(i+1))])
		if raw["data_state"] != "decoded" {
			t.Errorf("%s: data_state %v", c.name, raw["data_state"])
		}
		if s := jsonText(raw["fwd"]); s != c.want {
			t.Errorf("%s: raw.fwd %s, want %s", c.name, s, c.want)
		}
	}
	if tf := rawOf(got["messages_v2/2"])["text_flags"]; tf != "fwd.from_name" {
		t.Errorf("hidden name: raw.text_flags %v, want fwd.from_name", tf)
	}
}
