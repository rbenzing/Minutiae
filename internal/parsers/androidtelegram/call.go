package androidtelegram

import (
	"strconv"
	"time"

	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/call"
)

// The phone-call action and its discard reasons (constructors.tsv: TLRPC.java:26192, 20934-20966 @A-HEAD).
const (
	idActionPhoneCall      = 0x80e11a7f // TL_messageActionPhoneCall
	idReasonHangup         = 0x57adc690
	idReasonMissed         = 0x85e42301
	idReasonMigrateConf    = 0x9fbbf1f7 // carries a slug
	idReasonDisconnect     = 0xe095c1a0
	idReasonBusy           = 0xfaf7e8c9
	stateReasonNotRecorded = "not-recorded"
	stateNotUserDialog     = "not-user-dialog"
	basisStartPlusDuration = "start+duration"
	unitUnestablished      = "unestablished"
)

// reasonLabel names a discard reason ID (spec 8.7); "" for an ID the table does not name (the AllowGroupCall reason,
// whose key is never read).
func reasonLabel(id uint32) string {
	switch id {
	case idReasonMissed:
		return "missed"
	case idReasonBusy:
		return "busy"
	case idReasonHangup:
		return "hangup"
	case idReasonDisconnect:
		return "disconnect"
	case idReasonMigrateConf:
		return "migrate_conference_call"
	}
	return ""
}

// isPhoneCall says the row is a service message whose action was reached and is the phone-call action (spec 8.7): a
// blob that stops before or inside the action is never a call (it stays a message, kindOf).
func isPhoneCall(v view, kind string) bool {
	id, ok := v.ID("action")
	return ok && id == idActionPhoneCall && kind == "system"
}

// callOf is the call of a phone-call action (spec 8.7, Q5, Q6): direction as for a message; outcome missed for a
// Missed reason and rejected for a Busy one of an incoming call, unknown for every other reason or direction (the
// reason is kept raw, never an answered inferred from a duration); kind video or voice; duration_s and the end time
// only when facts.tsv unit.call_duration pins seconds (else raw only, with its unit); the address the dialog's user.
// start is the date column's time (nil when the date was not read as a valid time). It returns the call and the
// record's end time; raw is the row's raw, to which the action's fields are added.
func callOf(v view, dir string, d dialog, uid int64, uidOK bool, start *records.Time, raw map[string]any) (call.Call, *records.Time) {
	if f, ok := v.Int("action.flags"); ok {
		raw["flags"] = f
	}
	if id, ok := v.Int("action.call_id"); ok {
		raw["call_id"] = id
	}
	video := v.True("action.video")
	if video {
		raw["video"] = true
	}
	c := call.Call{Direction: dir, Outcome: call.OutcomeUnknown, Kind: call.KindVoice, Service: channelTelegram}
	if video {
		c.Kind = call.KindVideo
	}
	reason, hasReason := v.ID("action.reason")
	if hasReason {
		raw["reason"] = hexID(reason)
	} else {
		raw["reason_state"] = stateReasonNotRecorded
	}
	switch label := reasonLabel(reason); {
	case !hasReason:
	case label == "missed" && dir == call.DirIn:
		c.Outcome = call.OutcomeMissed
	case label == "busy" && dir == call.DirIn:
		c.Outcome = call.OutcomeRejected
	case label != "":
		raw["label_reason"] = label
	}
	if b, ok := v.Str("action.reason.slug"); ok {
		if s, ok := ident(raw, "slug", b, maxIdent); ok && s != "" {
			raw["slug"] = s
		}
	}
	var end *records.Time
	if n, ok := v.Int("action.duration"); ok {
		raw["duration"] = n
		if unitCallDuration != "seconds" {
			raw["duration_unit"] = unitUnestablished
		} else if n >= 0 {
			secs := float64(n)
			c.DurationS = &secs
			if n > 0 && start != nil {
				end = &records.Time{T: start.T.Add(time.Duration(n) * time.Second), Basis: start.Basis}
				raw["time_end_basis"] = basisStartPlusDuration
			}
		}
	}
	if d.class == classUser && uidOK {
		c.Address = string(classUser) + ":" + strconv.FormatInt(uid, 10)
	} else {
		raw["address_state"] = stateNotUserDialog
	}
	c.Raw = raw
	return c, end
}
