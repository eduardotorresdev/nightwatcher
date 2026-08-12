package ingest

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// HeaderSignature and HeaderResource are the delivery headers the nightwatcher reads.
// Sentry's own Node example reads them lowercased because Node normalises header names;
// HTTP header names are case-insensitive and http.Header.Get canonicalises, so the two
// spellings in circulation are the same header. Never index the map by hand.
const (
	HeaderSignature = "Sentry-Hook-Signature"
	HeaderResource  = "Sentry-Hook-Resource"
)

// Verify reports whether sig is the correct HMAC-SHA256, in hex, of body under secret.
//
// body must be the RAW BYTES read off the socket. Sentry signs the exact string it puts
// on the wire, so unmarshalling and remarshalling before this call produces a different
// message and the signature fails — Go's encoding/json and Python's json.dumps disagree
// on spacing and on unicode escaping. Read raw, verify, then parse.
//
// An empty secret always fails. A deployment that forgot SENTRY_CLIENT_SECRET must
// refuse everything rather than authenticate everything, which is what comparing
// against the HMAC of the empty key would amount to.
//
// The timestamp Sentry sends alongside is NOT part of the signed message, so none of
// this protects against replay. Dedup is what does.
func Verify(secret string, body, sig []byte) bool {
	if secret == "" {
		return false
	}
	// Decoding the hex, rather than comparing hex strings, buys the case-insensitivity
	// for free and rejects a wrong-length signature before any comparison happens.
	if len(sig) != hex.EncodedLen(sha256.Size) {
		return false
	}
	want := make([]byte, sha256.Size)
	if _, err := hex.Decode(want, sig); err != nil {
		return false
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	// hmac.Equal and never ==: a byte-by-byte comparison that returns early leaks how
	// much of a forged signature was right, which is enough to build the rest of it.
	return hmac.Equal(mac.Sum(nil), want)
}
