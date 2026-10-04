package records

import "github.com/rbenzing/minutiae/internal/evidence"

// Test exports of the unexported write-time validation.

type (
	ArtifactInfo = artifactInfo
	Prepared     = prepared
)

var (
	Prepare          = prepare
	CanonicalPayload = canonicalPayload
	CanonicalValue   = canonicalValue
	ValidateParser   = validateParser
)

// UnregisterType removes a type registered by a test.
func UnregisterType(name string) {
	registryMu.Lock()
	defer registryMu.Unlock()
	delete(registry, name)
}

const (
	MaxSummary      = maxSummary
	MaxBody         = maxBody
	MaxPayload      = maxPayload
	MaxLocator      = maxLocator
	MaxSourcePath   = maxSourcePath
	MaxPayloadDepth = maxPayloadDepth
	MaxTimes        = maxTimes
)

// Row returns the prepared evidence row.
func (p prepared) Row() evidence.RecordRow { return p.row }

// ApproxBytes returns the writer's size estimate of the row.
func (p prepared) ApproxBytes() int { return p.approxBytes }
