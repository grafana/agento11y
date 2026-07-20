package agento11y

import (
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"time"
)

func newRandomID(prefix string) string {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return prefix + "_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	}

	return prefix + "_" + hex.EncodeToString(raw[:])
}
