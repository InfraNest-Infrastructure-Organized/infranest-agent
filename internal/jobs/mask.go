package jobs

import (
	"regexp"
	"strings"
)

// Masked is what a secret is replaced with.
const Masked = "[masked]"

// secrets are the shapes masked in a job's output before it leaves the machine (InfraNest #5801). The same list
// InfraNest applies to a ticket it files (`FindingTicketBody::clean()`), so a line is masked the same way at both
// ends — and InfraNest masks it again when it arrives, for a sender that does not.
//
// Only the `name = value` shape keeps its name, so the reader still knows what was there.
var secrets = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----.*`),
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}\b`),
	regexp.MustCompile(`\blin_(?:api|oauth)_[A-Za-z0-9]{20,}\b`),
	regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}\b`),
	regexp.MustCompile(`\b(?:sk|rk|pk)_(?:live|test)_[A-Za-z0-9]{16,}\b`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	// Provider tokens with a prefix of their own (InfraNest #5867), before the generic long-run rules below — which
	// would otherwise take a token's tail and leave its prefix, or swallow part of the host in front of it.
	regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`\bwhsec_[A-Za-z0-9+/=]{20,}`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{30,}`),
	regexp.MustCompile(`\bnpm_[A-Za-z0-9]{30,}`),
	regexp.MustCompile(`\bdop_v1_[a-f0-9]{64}\b`),
	// A Slack incoming webhook: the path is the credential, the host stays.
	regexp.MustCompile(`(hooks\.slack\.com/services/)\S+`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`),
	// A password in a URL: scheme://user:secret@host, the user possibly empty (redis://:secret@host).
	regexp.MustCompile(`(://[^\s:/@]*:)[^\s@/]+@`),
	// An Authorization header's credential.
	regexp.MustCompile(`(?i)\b((?:authorization|proxy-authorization)\s*:\s*(?:bearer|basic|token)?\s*)\S+`),
	regexp.MustCompile(`(?i)\b((?:pass(?:word)?|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key)\s*[:=]\s*)\S+`),
	// A long unbroken run of key material: 32+ hex, or 40+ base64.
	regexp.MustCompile(`\b[A-Fa-f0-9]{32,}\b`),
	regexp.MustCompile(`[A-Za-z0-9+/]{40,}={0,2}`),
}

// Mask replaces everything secret-shaped in one line of output.
func Mask(line string) string {
	for _, re := range secrets {
		line = re.ReplaceAllStringFunc(line, func(m string) string {
			sub := re.FindStringSubmatch(m)
			if len(sub) > 1 && sub[1] != "" {
				if strings.HasPrefix(sub[1], "://") {
					return sub[1] + Masked + "@"
				}

				return sub[1] + Masked
			}

			return Masked
		})
	}

	return line
}

// MaskLines masks a run of lines, and everything between a private key's first and last line.
func MaskLines(lines []string) []string {
	out := make([]string, 0, len(lines))
	inKey := false
	for _, l := range lines {
		switch {
		case inKey:
			if strings.Contains(l, "-----END ") {
				inKey = false
			}
			out = append(out, Masked)
		case strings.Contains(l, "PRIVATE KEY-----") && strings.Contains(l, "-----BEGIN "):
			inKey = !strings.Contains(l, "-----END ")
			out = append(out, Mask(l))
		default:
			out = append(out, Mask(l))
		}
	}

	return out
}
