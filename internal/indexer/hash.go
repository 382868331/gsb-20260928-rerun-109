package indexer

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
)

func newSHA256() hash.Hash                             { return sha256.New() }
func copyHash(h hash.Hash, r io.Reader) (int64, error) { return io.Copy(h, r) }
func sumHex(h hash.Hash) string                        { return hex.EncodeToString(h.Sum(nil)) }
