// Package ocitransfer provides streaming blob transfers through oci.Reader and
// oci.Writer implementations. Downloads use parallel range reads and verify
// content at EOF. Uploads write sequential chunks and commit their digest.
package ocitransfer
