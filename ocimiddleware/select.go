package ocimiddleware

import "github.com/ohseeeye/oci"

// Select returns a wrapper for r that provides only
// repositories for which allow returns true.
//
// Requests for disallowed repositories will return ErrNameUnknown
// errors on read and ErrDenied on write.
func Select(r oci.Registry, allow func(repoName string) bool) oci.Registry {
	return AccessChecker(r, func(repoName string, access AccessKind) error {
		if allow(repoName) {
			return nil
		}
		if access == AccessWrite {
			return oci.ErrDenied
		}
		if access == AccessList && repoName == "*" {
			return nil
		}
		return oci.ErrNameUnknown
	})
}
