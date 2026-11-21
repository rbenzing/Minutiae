package androidtelegram

// Go constants that follow a pinned fact of tools/fixtures/schemas/telegram-android/facts.tsv (4I4-P4); each is
// asserted equal to its row by TestTelegramFactsMatchTheCode, so a fact and the code that follows it never drift.
const (
	// unitEditDate is facts.tsv unit.edit_date: the pinned source never establishes the unit of a message's
	// edit_date, so it stays raw with raw.edit_date_unit and never becomes Times.edited (spec 8.5, 10, T6 Q3).
	unitEditDate = "unestablished"
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
