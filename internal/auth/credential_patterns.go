package auth

// DefaultCredentialPatterns returns independent, additive rules for common
// credential formats. Go's regexp engine is linear-time; fixed provider prefixes
// and lengths keep ordinary identifiers and prose from being treated as keys.
// These are format checks, not proof that a value is a live credential.
func DefaultCredentialPatterns() []string {
	return []string{
		`cwk_[a-fA-F0-9]{64}`,
		`sk-[A-Za-z0-9_-]{20,}`,
		`[0-9]{6,12}:[A-Za-z0-9_-]{30,}`,
		`\bgh[pousr]_[A-Za-z0-9]{36}\b`,
		`\bgithub_pat_[A-Za-z0-9_]{82}\b`,
		`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`,
		`\bAIza[A-Za-z0-9_-]{35}`,
		`\bxox[baprs]-(?:[0-9]{1,16}-){1,3}[A-Za-z0-9]{16,64}\b`,
		`\beyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{16,}`,
	}
}
