package model

import "strings"

// GlobalIdentity is the reserved identity of the global project, whose
// entries every project can read. Git identities always contain a slash, so
// no repository can resolve to it.
const GlobalIdentity = "elephant:global"

// PinnedTag marks a global entry that recall shows in every project.
const PinnedTag = "pinned"

// OriginEdge links a global entry to the project it was written from.
const OriginEdge = "origin"

// ProjectNode returns the graph node naming a project by identity.
func ProjectNode(identity string) string { return "project:" + identity }

// ProjectFromNode returns the identity of a project node.
func ProjectFromNode(node string) (string, bool) { return strings.CutPrefix(node, "project:") }
