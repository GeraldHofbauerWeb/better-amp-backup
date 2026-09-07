package backup

import (
	"crypto/rand"
	"encoding/hex"
)

// randomSuffix produces the six hex characters that make a snapshot ID unique
// within the same second. Two backups of different instances can and do start
// on the same scheduler tick.
func randomSuffix() string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is not something a backup tool can paper over
		// with a weaker source: an ID collision would overwrite a snapshot.
		panic("backup: cannot read random bytes for snapshot id: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}
