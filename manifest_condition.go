package oci

import (
	"fmt"
	"strings"
)

// CheckManifestIfMatch compares an HTTP If-Match value with the current tag's
// manifest digest. An empty digest means the tag is absent. Empty value means
// unconditional; "*" requires an existing tag. Entity tags use strong comparison
// against the quoted digest, so weak validators never match. Malformed syntax
// returns ErrManifestInvalid; a valid but unsatisfied condition returns
// ErrPreconditionFailed. Backends must call this while holding the same lock or
// transaction used to update the tag, before changing manifests or tag history.
func CheckManifestIfMatch(value string, current Digest) error {
	if value == "" {
		return nil
	}
	value = strings.Trim(value, " \t")
	if value == "*" {
		if current != "" {
			return nil
		}
		return ErrPreconditionFailed
	}
	invalid := func() error { return fmt.Errorf("%w: malformed If-Match", ErrManifestInvalid) }
	matched, found := false, false
	for len(value) > 0 {
		// HTTP list syntax permits empty list members between commas.
		value = strings.TrimLeft(value, " \t,")
		if value == "" {
			break
		}
		weak := strings.HasPrefix(value, "W/")
		if weak {
			value = value[2:]
		}
		if value == "" || value[0] != '"' {
			return invalid()
		}
		value = value[1:]
		end := strings.IndexByte(value, '"')
		if end < 0 {
			return invalid()
		}
		for i := range end {
			c := value[i]
			if c < 0x21 || c == 0x7f {
				return invalid()
			}
		}
		if !weak && current != "" && value[:end] == current.String() {
			matched = true
		}
		found = true
		value = strings.TrimLeft(value[end+1:], " \t")
		if value != "" && value[0] != ',' {
			return invalid()
		}
	}
	if !found {
		return invalid()
	}
	if !matched {
		return ErrPreconditionFailed
	}
	return nil
}
