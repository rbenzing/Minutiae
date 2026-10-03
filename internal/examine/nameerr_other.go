//go:build !windows

package examine

func isLocalNameErrorOS(error) bool { return false }
