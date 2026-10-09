package evidence

// MaxGapNoteRuns is how many overlap runs an AcquisitionGapNote lists.
const MaxGapNoteRuns = 8

// AcquisitionGapNote says that a derived artifact's bytes were read from media
// ranges the acquirer could not read and zero-filled (E01 error2): those zero
// bytes are acquirer fill, not device data. Bytes and Ranges are the exact
// overlap of the artifact's recorded image runs with the declared ranges; First
// lists the first overlaps (image coordinates). Unknown means the overlap could
// not be decided (the runs are not recorded, the content is stored inline in
// filesystem metadata, or the image declares more ranges than it keeps): it is
// never read as "none".
type AcquisitionGapNote struct {
	Bytes   int64 `json:"bytes"`
	Ranges  int   `json:"ranges"`
	First   []Run `json:"first,omitempty"`
	Unknown bool  `json:"unknown,omitempty"`
}
