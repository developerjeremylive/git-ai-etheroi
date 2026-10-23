// Package refchange parses the reference updates that Git gives its hooks. It has no dependency
// beyond the standard library, so that clients built for any platform can use it.
package refchange

import "strings"

// ParseHookLine parses one line of the input of the reference-transaction,
// pre-receive and post-receive hooks, "<old-value> <new-value> <reference>",
// without its newline. Git separates the fields with single ASCII spaces and
// the reference is everything after the second one, kept verbatim: reference
// names may hold any byte that is not ASCII, including Unicode whitespace such
// as U+00A0 and bytes that are not valid UTF-8, so the line must not be split on
// whitespace in general. A trailing carriage return is ignored.
//
// ok is false when the line does not have this form: when a field is missing or
// empty, or when a field holds an ASCII space or control character, which Git
// does not allow in reference names and object IDs.
func ParseHookLine(line string) (oldValue, newValue, reference string, ok bool) {
	parts := strings.SplitN(strings.TrimSuffix(line, "\r"), " ", 3)
	if len(parts) != 3 {
		return "", "", "", false
	}

	for _, part := range parts {
		if part == "" || strings.ContainsFunc(part, isASCIISpaceOrControl) {
			return "", "", "", false
		}
	}

	return parts[0], parts[1], parts[2], true
}

// isASCIISpaceOrControl reports whether r is the ASCII space or an ASCII control
// character. Bytes that are not valid UTF-8 decode to utf8.RuneError, which is
// neither.
func isASCIISpaceOrControl(r rune) bool {
	return r <= ' ' || r == 0x7f
}
