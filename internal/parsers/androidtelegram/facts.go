package androidtelegram

// Go constants that follow a pinned fact of tools/fixtures/schemas/telegram-android/facts.tsv (4I4-P4); each is
// asserted equal to its row by TestTelegramFactsMatchTheCode, so a fact and the code that follows it never drift.
const (
	// unitEditDate is facts.tsv unit.edit_date (plan 4I4b Task 1, rulings R5, B3): seconds at all four pins, so a
	// message's edit_date is Times.edited (editTime) beside raw.edit_date and raw.edit_date_unit. The call site
	// differs by pin: at A-HEAD the cell passes edit_date to timeString (ChatMessageCell.java:15657), whose edited
	// label formats it with LocaleController.formatPmEditedDate (ChatMessageCell.java:15576); at A-11.4.2,
	// release-8.1.1_2431 and release-5.14.0_1851 the cited call site is the cell's own
	// LocaleController.formatLocationUpdateDate call on edit_date (ChatMessageCell.java:8330, 4968, 4050), the
	// formatter named for a location's update date, not the edited label route. Both formatters multiply the value by
	// 1000 before Calendar.setTimeInMillis, which establishes seconds at each pin. Any other value leaves edit_date
	// raw only.
	unitEditDate = "seconds"
	// unitBlobTimes is facts.tsv unit.blob_times: the unit of the blob's other times (fwd_from.date, until_date,
	// expire_date) is not established as a set, so they stay raw only (spec 10).
	unitBlobTimes = "unestablished"
	// encChatIDSigned is facts.tsv cast.encrypted_chat_id "int32": the enc_chats key of a secret dialog is the low 32
	// bits of its id cast to int (DO:117-119), so a chat id above 2^31-1 is negative (R10: -5), never re-signed
	// (4I4-P12, encChatID).
	encChatIDSigned = true
)

// unitCallDuration is facts.tsv unit.call_duration: the pinned source formats a phone-call action's duration as
// seconds (up to 60, then minutes), so duration_s is the stored value and Record.TimeEnd is start plus it (spec 8.7,
// 10, T1 c4). Any other value would leave the duration in raw with raw.duration_unit "unestablished".
const unitCallDuration = "seconds"

// putReinserts is facts.tsv write_verb.*.put: every pinned put of a message table is REPLACE INTO, which deletes a
// stored (mid, uid) row and inserts the new one (a new rowid in messages_v2, F30), so an older image of a live
// message is a row the live table no longer holds: the typed pass states each recovered message's own key against the
// live rows as raw.live_key_state (ruling T2). An UPDATE everywhere would leave the older image to the host's
// superseded-version relation and the key state unneeded.
const putReinserts = true
