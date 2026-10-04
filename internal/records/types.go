package records

import (
	"fmt"
	"regexp"
	"sync"
)

// Type is a registered record type. Validate, when set, checks a payload's
// contract (each type's required fields belong to sub-project 4); an error from
// it is reported wrapping ErrInvalidPayload.
type Type struct {
	Name           string
	PayloadVersion int
	Validate       func(payload map[string]any) error
}

var (
	typeNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{1,31}$`)

	registryMu sync.RWMutex
	registry   = map[string]Type{}
)

// coreTypes are registered at init with no payload validator.
var coreTypes = []string{
	"message", "call", "contact", "calendar_event", "location", "web_visit",
	"web_search", "download", "file", "account", "app_event", "media", "note",
	"wifi_network", "event",
}

func init() {
	for _, n := range coreTypes {
		if err := RegisterType(Type{Name: n, PayloadVersion: 1}); err != nil {
			panic(err) // a programming error in coreTypes
		}
	}
}

// RegisterType adds a record type. The name must match [a-z][a-z0-9_]{1,31},
// PayloadVersion must be at least 1 and the name must not be registered yet.
func RegisterType(t Type) error {
	if !typeNameRE.MatchString(t.Name) {
		return fmt.Errorf("%w: type name %q must match [a-z][a-z0-9_]{1,31}", ErrInvalidField, t.Name)
	}
	if t.PayloadVersion < 1 {
		return fmt.Errorf("%w: type %q: payload version %d must be at least 1", ErrInvalidField, t.Name, t.PayloadVersion)
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[t.Name]; dup {
		return fmt.Errorf("%w: type %q is already registered", ErrInvalidField, t.Name)
	}
	registry[t.Name] = t
	return nil
}

// LookupType returns the registered type with that name.
func LookupType(name string) (Type, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	t, ok := registry[name]
	return t, ok
}
