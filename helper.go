package cmap

func assert1(guard bool, text string) {
	if !guard {
		panic(text)
	}
}

// nextPowerOfTwo returns the next power of two greater than or equal to x
func nextPowerOfTwo(x int) int {
	if x <= 0 {
		return 1
	}
	if x >= maximumCapacity {
		return maximumCapacity
	}
	// Find the smallest power of 2 >= x
	x--
	x |= x >> 1
	x |= x >> 2
	x |= x >> 4
	x |= x >> 8
	x |= x >> 16
	return x + 1
}

// numberOfLeadingZeros returns the number of leading zero bits in x
func numberOfLeadingZeros(x uint32) int {
	if x == 0 {
		return 32
	}
	n := 0
	if (x & 0xFFFF0000) == 0 {
		n += 16
		x <<= 16
	}
	if (x & 0xFF000000) == 0 {
		n += 8
		x <<= 8
	}
	if (x & 0xF0000000) == 0 {
		n += 4
		x <<= 4
	}
	if (x & 0xC0000000) == 0 {
		n += 2
		x <<= 2
	}
	if (x & 0x80000000) == 0 {
		n += 1
	}
	return n
}
