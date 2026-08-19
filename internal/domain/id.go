package domain

import (
	"crypto/sha256"
	"encoding/hex"
)

func SessionDigest(harness, nativeSessionID, canonicalRoot string) string {
	return digest("session\x00" + harness + "\x00" + nativeSessionID + "\x00" + canonicalRoot)
}

func SessionReferenceDigest(reference SessionReference) string {
	identity := reference.NativeSessionID
	if identity == "" {
		identity = reference.CanonicalSourceIdentity
	}
	return SessionDigest(reference.HarnessID, identity, reference.CanonicalSourceRoot)
}

func EventDigest(sessionID, nativeIdentity string) string {
	return digest("event\x00" + sessionID + "\x00" + nativeIdentity)
}

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
