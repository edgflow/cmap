package cmap

import (
	"fmt"
	"hash/fnv"
)

type Hasher interface {
	Hash(key any) uint64
}

type FNVHasher struct{}

func (h FNVHasher) Hash(key any) uint64 {
	hasher := fnv.New64()
	// Convert `any` to string using fmt.Sprintf
	s := fmt.Sprintf("%v", key)
	hasher.Write([]byte(s))
	return hasher.Sum64()
}
