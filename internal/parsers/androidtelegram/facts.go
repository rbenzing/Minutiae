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
)
