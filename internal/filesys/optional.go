package filesys

import "reflect"

// maxWrapperDepth bounds the walk of As.
const maxWrapperDepth = 64

// Wrapper is implemented by a filesystem that wraps another (such as the detect layer that attaches
// notes), so optional interfaces of the wrapped filesystem can still be found.
type Wrapper interface{ Underlying() FileSystem }

// As returns the first filesystem on the Underlying chain of fsys (fsys itself first) that implements
// T. The walk ends at a nil filesystem (a typed nil too), a repeated one (a cycle) or after maxWrapperDepth steps.
func As[T any](fsys FileSystem) (T, bool) {
	var zero T
	seen := make(map[FileSystem]struct{})
	for range maxWrapperDepth {
		if isNilFS(fsys) {
			return zero, false
		}
		if t, ok := fsys.(T); ok {
			return t, true
		}
		if comparableFS(fsys) {
			if _, dup := seen[fsys]; dup {
				return zero, false
			}
			seen[fsys] = struct{}{}
		}
		w, ok := fsys.(Wrapper)
		if !ok {
			return zero, false
		}
		fsys = w.Underlying()
	}
	return zero, false
}

// comparableFS reports whether fsys can be a map key (an interface holding an uncomparable dynamic type
// would panic); an uncomparable one is simply not tracked, and the depth cap still bounds the walk.
func comparableFS(fsys FileSystem) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	_ = map[FileSystem]struct{}{fsys: {}}
	return true
}

// isNilFS reports whether fsys is nil or an interface holding a nil pointer (a typed nil, which a
// wrapper's Underlying may return): both end the chain, and a typed nil is never offered as a capability.
func isNilFS(fsys FileSystem) bool {
	if fsys == nil {
		return true
	}
	v := reflect.ValueOf(fsys)
	switch v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.Interface:
		return v.IsNil()
	}
	return false
}

// SupportsRecovery reports whether fsys can describe where the content of a deleted entry may be: a
// filesystem (or one it wraps) implements Recoverer. A wrapper that always has a Recoverable method but
// forwards to something that may lack one (the panic-protecting wrapper of internal/examine) says so
// with a SupportsRecovery() bool method, which wins. Callers use this, never a type assertion.
func SupportsRecovery(fsys FileSystem) bool {
	if s, ok := fsys.(interface{ SupportsRecovery() bool }); ok {
		return s.SupportsRecovery()
	}
	_, ok := As[Recoverer](fsys)
	return ok
}
