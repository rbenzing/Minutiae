package carve

// validateTIFF is a stub until its task lands (TIFF and BigTIFF): every hit is rejected.
func validateTIFF(*Window) Verdict {
	return Verdict{Outcome: Rejected, Reason: "no-validator"}
}
