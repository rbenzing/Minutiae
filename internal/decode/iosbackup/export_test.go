package iosbackup

// Kept is the number of Files rows ReadFiles retained.
func Kept(f *Files) int { return len(f.kept) }
