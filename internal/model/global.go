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

// HostTag prefixes the tags that limit a global entry to named machines.
const HostTag = "host:"

// HostName normalizes a machine name for host tags: lowercased, with every
// character other than a-z, 0-9, - _ . replaced by -, and cut to fit a tag.
func HostName(raw string) string {
	name := []byte(strings.ToLower(strings.TrimSpace(raw)))
	for i, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.IndexByte("-_.", c) >= 0) {
			name[i] = '-'
		}
	}
	return string(name[:min(len(name), maxTagLen-len(HostTag))])
}

// OnHost reports whether an entry with these tags applies on host: it has
// no host tag, or one of its host tags names host.
func OnHost(tags []string, host string) bool {
	limited := false
	for _, tag := range tags {
		if name, ok := strings.CutPrefix(tag, HostTag); ok {
			if name == host {
				return true
			}
			limited = true
		}
	}
	return !limited
}
