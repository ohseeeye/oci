// Package ocitransfer copies images, indexes, artifacts, and tagged repositories,
// and provides streaming blob transfers through OCI reader/writer interfaces.
// Downloads use parallel range reads and verify content at EOF. Uploads write
// sequential chunks and commit their digest. Copies preserve content digests and
// publish dependencies before parents.
package ocitransfer
