package carve

// Outcome is the class of a validator's verdict and of a hit.
type Outcome string

// The outcomes: validated (the structure proved the exact length), footer
// (a footer was found), bounded (only a cap or the run end limits the object)
// and rejected (no artifact).
const (
	Validated Outcome = "validated"
	Footer    Outcome = "footer"
	Bounded   Outcome = "bounded"
	Rejected  Outcome = "rejected"
)

// Verdict is a validator's answer for one header hit. Length is the exact
// object length for validated; Type and Ext refine the family (empty keeps
// the signature's own); Reason says why a hit is rejected, or, on a bounded
// verdict, why the object is only bounded (the scanner then requires Length to
// be the window, labels the cut and counts the reason in Stats.BoundedBy);
// Basis and Assumptions carry the provenance of the length.
type Verdict struct {
	Outcome            Outcome
	Length             int64
	Reason, Type, Ext  string
	Basis, Assumptions []string
}

// validatorFunc decides one hit by reading its window.
type validatorFunc func(w *Window) Verdict

// builtinValidators is the registry of the validator ids a Type may name.
// A fresh map on every call: the package keeps no mutable state.
func builtinValidators() map[string]validatorFunc {
	return map[string]validatorFunc{
		"jpeg":      validateJPEG,
		"png":       validatePNG,
		"gif":       validateGIF,
		"riff-webp": validateRIFFWebP,
		"isobmff":   validateBMFF,
		"sqlite":    validateSQLite,
		"bplist":    validateBPlist,
		"zip":       validateZIP,
		"pdf":       validatePDF,
		"gzip":      validateGzip,
		"riff-wave": validateRIFFWave,
		"ogg":       validateOgg,
		"bmp":       validateBMP,
		"tiff":      validateTIFF,
		"ico":       validateICO,
	}
}
