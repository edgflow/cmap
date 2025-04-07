package cmap

import (
	"fmt"
	"testing"
)

func TestFlurryHashMap_Get(t *testing.T) {
	f := NewFlurryHashMap()
	f.Put("hi", 1)
	f.Put("hi1", 2)
	fmt.Println(f.Get("hi"))
}
