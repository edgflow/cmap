package cmap

import (
	"fmt"
	"testing"
)

func TestFlurryHashMap_Get(t *testing.T) {
	f := NewCmap()
	f.Put("hi", 1)
	fmt.Println(f.Put("hi", 2))
	f.Put("hi1", 2)
	fmt.Println(f.Get("hi"))
}
