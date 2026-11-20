// Package skills exposes the Agent Skill that ships with repath.
//
// The canonical file is skills/repath/SKILL.md. It is embedded into the binary
// so `repath install` works from a copied binary with no repo checkout and no
// network access.
package skills

import _ "embed"

// Repath is the bytes of skills/repath/SKILL.md.
//
//go:embed repath/SKILL.md
var Repath []byte
