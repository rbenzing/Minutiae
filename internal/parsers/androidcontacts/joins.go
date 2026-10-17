package androidcontacts

import (
	"errors"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/recordtypes/contact"
)

// The account and the time of a raw contact live in other tables of the provider's database:
//   - accounts, through raw_contacts.account_id = accounts._id (the account the contact belongs to);
//   - contacts, through raw_contacts.contact_id = contacts._id: contact_last_updated_timestamp is the
//     last update of the AGGREGATE contact, not of the raw contact (the provider keeps no time per raw
//     contact), so it is stated under contact.AggregateLastUpdated and never as Record.Time.
//
// Every outcome of a join is stated, never shown as an absent account or time:
//   - a NULL account_id is a contact without an account (a device-local contact): nothing is stated;
//   - an account_id with no accounts row is raw.account_state = "no-accounts-row" (a decided miss);
//   - a NULL contact_id is raw.aggregate_state = "no-contact-id", a contact_id with no contacts row
//     "no-contacts-row" (T72);
//   - a join the engine cannot decide (the table is missing, lacks its key, or its pages were lost) lists
//     the table in raw.join_undecided and states neither an account nor a miss;
//   - a cell of a joined row that is not the wanted storage class is listed under
//     raw.joined_unknown_columns, qualified by table (unknown_columns is reserved for the columns of the
//     mapped raw_contacts row).

const aggregateSource = "contacts.contact_last_updated_timestamp"

// joined records which joins could not be decided, so Parse lists them in a fixed order.
type joined struct{ data, accounts, contacts bool }

func (j joined) list() []any {
	var out []any
	for _, e := range []struct {
		on   bool
		name string
	}{{j.data, "data"}, {j.accounts, "accounts"}, {j.contacts, "contacts"}} {
		if e.on {
			out = append(out, e.name)
		}
	}
	return out
}

// matchKey finds the rows of table whose _id is id. undecided is true when the engine cannot answer: the
// table is absent or lacks _id, the index lost rows, or the key cannot be decided; any other error (the
// budget, a cancelled context, a corrupt structure) is returned and fails the job.
func matchKey(m parse.MapContext, table string, id int64) (rows []parse.Row, undecided bool, err error) {
	rows, undecided, err = rowrun.Match(m, table, "_id", id)
	var unsupported *sqlitedb.UnsupportedSchemaError
	switch {
	case errors.Is(err, sqlitedb.ErrNoSuchTable), errors.As(err, &unsupported):
		return nil, true, nil
	case err != nil:
		return nil, false, err
	}
	return rows, undecided, nil
}

// flagJoined records in joinedUnknown the cell of a joined row that holds no usable value, under the
// qualified name "<table>.<column>": the storage class, the state the decoder gave it, or a column the
// variant of that table lacks.
func flagJoined(joinedUnknown map[string]any, name string, row parse.Row, col int, k rowrun.CellKind) {
	if k == rowrun.CellMissing {
		joinedUnknown[name] = "column-missing"
		return
	}
	rowrun.Flag(joinedUnknown, row, name, col, k)
}

// joinedText reads a text cell of a joined row; NULL is an empty value, anything else that is not text is
// unknown and flagged.
func joinedText(row parse.Row, table, name string, col int, raw, joinedUnknown map[string]any) string {
	b, k := rowrun.TextCell(row, col)
	switch k {
	case rowrun.CellValue:
		return cleanField(raw, name, b)
	case rowrun.CellNull:
		return ""
	}
	flagJoined(joinedUnknown, table+"."+name, row, col, k)
	return ""
}

func account(typ, name string) *contact.Account {
	if typ == "" && name == "" {
		return nil
	}
	return &contact.Account{Type: typ, Name: name}
}

// accountOf returns the account of a raw contact. An older schema keeps account_name and account_type
// inline; they are honoured only when physically present. Otherwise the account is the accounts row
// raw_contacts.account_id names.
func accountOf(m parse.MapContext, row parse.Row, c rowrun.Cols, raw, unknown, joinedUnknown map[string]any, j *joined) (*contact.Account, error) {
	if c.At("account_name") >= 0 || c.At("account_type") >= 0 {
		typ := textOf(row, c, "account_type", raw, unknown)
		name := textOf(row, c, "account_name", raw, unknown)
		return account(typ, name), nil
	}
	aid, k := rowrun.IntCell(row, c.At("account_id"))
	switch k {
	case rowrun.CellValue:
	case rowrun.CellNull: // a device-local contact: no account
		return nil, nil
	case rowrun.CellMissing: // neither account_id nor the inline columns: this variant cannot say
		raw["account_state"] = "no-account-id-column"
		return nil, nil
	default:
		rowrun.Flag(unknown, row, "account_id", c.At("account_id"), k)
		return nil, nil
	}
	raw["account_id"] = aid
	rows, und, err := matchKey(m, "accounts", aid)
	if err != nil {
		return nil, err
	}
	switch {
	case len(rows) > 1: // one _id names at most one row: more is a corrupt table, and picking one would be a guess
		j.accounts = true
		return nil, nil
	case len(rows) == 0:
		if und {
			j.accounts = true
		} else {
			raw["account_state"] = "no-accounts-row"
		}
		return nil, nil
	}
	j.accounts = j.accounts || und // the row was found, but the answer may be short
	ar := rows[0]
	typ := joinedText(ar, "accounts", "account_type", m.Column("accounts", "account_type"), raw, joinedUnknown)
	name := joinedText(ar, "accounts", "account_name", m.Column("accounts", "account_name"), raw, joinedUnknown)
	return account(typ, name), nil
}

// aggregateTime states the last update of the aggregate contact of a raw contact: the raw value and its
// source in raw, the status of a sentinel or invalid value, and the time under contact.AggregateLastUpdated
// when it is valid. It returns the instant for the caller's Record.Times; stated is false when the join gave
// no value (the caller must then not read the instant: its zero value is not a time).
func aggregateTime(m parse.MapContext, row parse.Row, c rowrun.Cols, raw, unknown, joinedUnknown map[string]any, j *joined) (in rowrun.Instant, stated bool, err error) {
	cid, k := rowrun.IntCell(row, c.At("contact_id"))
	switch k {
	case rowrun.CellValue:
	case rowrun.CellNull: // not aggregated: the time is unknown, never zero
		raw["aggregate_state"] = "no-contact-id"
		return in, false, nil
	case rowrun.CellMissing:
		raw["aggregate_state"] = "no-contact-id-column"
		return in, false, nil
	default:
		rowrun.Flag(unknown, row, "contact_id", c.At("contact_id"), k)
		return in, false, nil
	}
	raw["contact_id"] = cid
	rows, und, err := matchKey(m, "contacts", cid)
	if err != nil {
		return in, false, err
	}
	switch {
	case len(rows) > 1:
		j.contacts = true
		return in, false, nil
	case len(rows) == 0:
		if und {
			j.contacts = true
		} else {
			raw["aggregate_state"] = "no-contacts-row"
		}
		return in, false, nil
	}
	j.contacts = j.contacts || und
	col := m.Column("contacts", "contact_last_updated_timestamp")
	v, tk := rowrun.IntCell(rows[0], col)
	switch tk {
	case rowrun.CellValue:
		in = rowrun.Millis(v)
		in.Record(raw, "contact_last_updated_timestamp")
		raw["contact_last_updated_source"] = aggregateSource
		stated = true
	case rowrun.CellNull: // the aggregate has no recorded update: nothing to state
	default:
		flagJoined(joinedUnknown, "contacts.contact_last_updated_timestamp", rows[0], col, tk)
	}
	return in, stated, nil
}
