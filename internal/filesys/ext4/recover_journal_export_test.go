package ext4

// SetJournalCopyCap lowers the number of journal copies of an inode-table block one entry examines (before
// PrepareJournalRecovery); a value of 0 or less leaves it as it is.
func (f *FS) SetJournalCopyCap(n int) {
	if n > 0 {
		f.jrecCopyCap = n
	}
}

// SetJournalStepCap lowers the work budget of one journal preparation (before PrepareJournalRecovery).
func (f *FS) SetJournalStepCap(n int64) {
	if n > 0 {
		f.jrecStepCap = n
	}
}
