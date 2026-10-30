package carve

// validateBMP is a stub until its task lands (BMP (and its DIB headers)): every hit is rejected.
func validateBMP(*Window) Verdict {
	return Verdict{Outcome: Rejected, Reason: "no-validator"}
}
