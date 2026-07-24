package main

import (
	"regexp"
	"strings"
)

// MCX Step 2 — MCX-style identity (user@domain), additive over the existing
// callsign + agency model. The callsign remains the display name and the
// audio/roster/key routing key; the MCX id is a derived, standards-aligned
// identifier for federation / future SIP-gateway interop. Nothing here changes
// how audio is routed today.

// agencyDomain returns the MCX domain for an agency: <agencyID>.<mcptt_domain>.
// A blank mcpttDomain (unconfigured) yields just the agency id so ids are still
// well-formed locally.
func agencyDomain(agencyID, mcpttDomain string) string {
	agencyID = mcxLabel(agencyID)
	if agencyID == "" {
		agencyID = "default"
	}
	if mcpttDomain == "" {
		return agencyID
	}
	return agencyID + "." + mcpttDomain
}

// userMCXID builds a user's MCX identity: <callsign>@<agencyID>.<mcptt_domain>.
// The local part is the lowercased, sanitized callsign (display name is kept
// separately and unchanged).
func userMCXID(callsign, agencyID, mcpttDomain string) string {
	return mcxLabel(callsign) + "@" + agencyDomain(agencyID, mcpttDomain)
}

// groupMCXID builds a group's MCX identity: <groupName>@<agencyID>.<mcptt_domain>.
func groupMCXID(groupName, agencyID, mcpttDomain string) string {
	return mcxLabel(groupName) + "@" + agencyDomain(agencyID, mcpttDomain)
}

// mcxLabel normalizes a name into a DNS/SIP-safe label: lowercase, spaces and
// invalid characters replaced with '-', collapsed. Keeps display names intact
// elsewhere; only the MCX-id local/agency labels are sanitized.
func mcxLabel(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
			lastDash = false
		} else {
			// Any separator (space, dot, punctuation) collapses to a single '-'.
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-._")
}

// mcxIDPattern matches user@domain where domain has at least one dot-separated
// label. Deliberately permissive on the local part (callsign-derived).
var mcxIDPattern = regexp.MustCompile(`^[a-z0-9._-]+@[a-z0-9.-]+$`)

// validMCXID reports whether id is a syntactically valid MCX identity.
func validMCXID(id string) bool {
	if !mcxIDPattern.MatchString(id) {
		return false
	}
	at := strings.IndexByte(id, '@')
	// must have a non-empty local part and a domain with at least one dot or a
	// bare agency label (unconfigured-domain case).
	return at > 0 && at < len(id)-1
}

// mcxDomainMatches reports whether an MCX id's domain belongs to this server's
// configured mcptt_domain (any agency subdomain of it). Used to reject foreign
// identities at registration. A blank mcpttDomain accepts anything (local mode).
func mcxDomainMatches(id, mcpttDomain string) bool {
	if mcpttDomain == "" {
		return true
	}
	at := strings.IndexByte(id, '@')
	if at < 0 {
		return false
	}
	domain := id[at+1:]
	return domain == mcpttDomain || strings.HasSuffix(domain, "."+mcpttDomain)
}
