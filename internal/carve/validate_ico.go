package carve

// validateICO is a stub until its task lands (ICO and CUR): every hit is rejected.
func validateICO(*Window) Verdict {
	return Verdict{Outcome: Rejected, Reason: "no-validator"}
}
