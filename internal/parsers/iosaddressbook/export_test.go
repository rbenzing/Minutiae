package iosaddressbook

// WithPhotoCap is a Parser whose photo-presence set holds at most n ids (a test seam).
func WithPhotoCap(n int) Parser { return Parser{photoCap: n} }
