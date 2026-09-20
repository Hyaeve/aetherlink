package upstream

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
)

func PlaybackCacheScope(ctx context.Context) string {
	identity := contextClientIdentity(ctx)
	sum := sha256.Sum256([]byte(identity.Token + "\x00" + identity.Session + "\x00" + contextUserAgent(ctx)))
	return hex.EncodeToString(sum[:])
}
