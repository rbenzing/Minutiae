package carvetest

// Image returns size bytes of fill repeated from its start. An empty fill
// gives zero bytes.
func Image(size int, fill []byte) []byte {
	img := make([]byte, size)
	if len(fill) == 0 {
		return img
	}
	for i := range img {
		img[i] = fill[i%len(fill)]
	}
	return img
}

// Place copies obj into img at off. It panics when obj does not fit: a test
// that places an object outside its image is a test bug.
func Place(img []byte, off int, obj []byte) {
	if off < 0 || off+len(obj) > len(img) {
		panic("carvetest: object does not fit the image")
	}
	copy(img[off:], obj)
}
