package ioswacontacts

import (
	"errors"
	"fmt"
	"io"

	"github.com/rbenzing/minutiae/internal/decode/plist"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
)

// The app label (4I2-R5, spec 6.8) comes from structure only: the backup domain names the app; at an app group the
// container metadata plist names it through MCMMetadataIdentifier (a key from memory, docs/limitations.md). Nothing in
// the database (partner names, JIDs, table content) ever labels the app.

const (
	maxContainerMeta = 64 << 10 // the most bytes of the metadata plist that are read
	metaKey          = "MCMMetadataIdentifier"
	metaIDConsumer   = "group.net.whatsapp.WhatsApp.shared"
	metaIDBusiness   = "group.net.whatsapp.WhatsAppSMB.shared"
	maxIdentifier    = 256
)

// appLabel is the app of a database and how that was decided. App is "whatsapp", "whatsapp_business" or "unknown"; Basis
// is set only for a known app; State only for "unknown" (the vocabulary of 4I2-R7). Identifier is the cleaned stored
// identifier of a metadata-other-identifier, MetaError the kind of an unreadable metadata plist.
type appLabel struct{ App, Basis, State, Identifier, MetaError string }

// labelOf labels the database at place p. At a domain the label is the domain's and the metadata is never read; at an
// app group the container_meta artifact is read, at most maxContainerMeta bytes of it. A read that fails is an error
// (an I/O fact is no label), never an unknown app.
func labelOf(in *parse.Input, p dbPlace) (appLabel, error) {
	if p.Place == placeDomain {
		return appLabel{App: p.App, Basis: "backup_domain"}, nil
	}
	unknown := func(state, kind string) (appLabel, error) {
		return appLabel{App: "unknown", State: state, MetaError: kind}, nil
	}
	a, ok := in.Artifacts["container_meta"]
	if !ok {
		return unknown("no-container-metadata", "")
	}
	if a.R == nil {
		return appLabel{}, errors.New(Name + ": the container_meta artifact has no reader")
	}
	if a.Size > maxContainerMeta {
		return unknown("metadata-unreadable", "too-large")
	}
	// the host's size is what the manifest recorded: read one byte more, so a longer file is still too large
	buf, err := io.ReadAll(io.NewSectionReader(a.R, 0, maxContainerMeta+1))
	if err != nil {
		return appLabel{}, fmt.Errorf("%s: reading the container metadata: %w", Name, err) // an I/O fact, never a label
	}
	if len(buf) > maxContainerMeta {
		return unknown("metadata-unreadable", "too-large")
	}
	m := &meter{b: in.Budget}
	defer m.release()
	v, err := plist.Decode(buf, m)
	if err != nil {
		return unknown("metadata-unreadable", plistFailure(err))
	}
	dict, ok := v.(map[string]any)
	if !ok {
		return unknown("metadata-unreadable", "not-a-dictionary")
	}
	id, ok := dict[metaKey].(string)
	if !ok {
		return unknown("metadata-no-identifier", "")
	}
	switch id {
	case metaIDConsumer:
		return appLabel{App: "whatsapp", Basis: "container_metadata"}, nil
	case metaIDBusiness:
		return appLabel{App: "whatsapp_business", Basis: "container_metadata"}, nil
	}
	l, _ := unknown("metadata-other-identifier", "")
	l.Identifier = common.CleanText([]byte(id), maxIdentifier).Text
	return l, nil
}

// note writes the label as job notes (before rowrun.Run: the first write of a key wins).
func (l appLabel) note(out parse.Emitter, p dbPlace) {
	if p.Place == placeDomain {
		out.Note("container", "backup_domain")
	} else {
		out.Note("container", "app_group")
	}
	raw := map[string]any{}
	l.raw(raw)
	for _, k := range []string{"app", "app_basis", "app_state", "container_meta_error", "container_identifier"} {
		if s, ok := raw[k].(string); ok {
			out.Note(k, s)
		}
	}
}

// raw states the label in a record's raw object: the same keys as the notes, an absent one absent.
func (l appLabel) raw(raw map[string]any) {
	raw["app"] = l.App
	if l.Basis != "" {
		raw["app_basis"] = l.Basis
	}
	if l.State != "" {
		raw["app_state"] = l.State
	}
	if l.MetaError != "" {
		raw["container_meta_error"] = l.MetaError
	}
	if l.State == "metadata-other-identifier" {
		raw["container_identifier"] = l.Identifier
	}
}

// meter is a plist.Budget that charges the host's view and remembers what is still held, so a successful decode (whose
// charge the decoder never returns) is given back as soon as the facts are copied out (restated from iossms).
type meter struct {
	b   *parse.BudgetView
	net int64
}

func (m *meter) Alloc(n int64) error {
	if err := m.b.Alloc(n); err != nil {
		return err
	}
	m.net += n
	return nil
}

func (m *meter) Free(n int64) {
	m.net -= n
	m.b.Free(n)
}

func (m *meter) release() { m.b.Free(m.net); m.net = 0 }

// plistFailure classifies a decode error: "budget" when the host refused the memory (a fact about this run, not about
// the data), otherwise "undecodable" (malformed, over a limit, unsupported).
func plistFailure(err error) string {
	if errors.Is(err, plist.ErrNoBudget) {
		return "budget"
	}
	return "undecodable"
}
