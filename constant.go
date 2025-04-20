package cmap

// Constants for the hash map implementation
const (
	// MaximumCapacity is the largest possible table capacity.
	// Must be exactly 1<<30 to stay within array allocation bounds
	// for power of two table sizes
	MaximumCapacity = 1 << 30

	// DefaultCapacity is the default initial table capacity.
	// Must be a power of 2 and at most MaximumCapacity.
	DefaultCapacity = 16

	// LoadFactor is the load factor for this table.
	// Used for calculating resize thresholds.
	LoadFactor = 0.75

	// MinTransferStride is the minimum number of rebinnings per transfer step.
	// Ranges are subdivided to allow multiple resizer threads.
	MinTransferStride = 16

	// ResizeStampBits is the number of bits used for generation stamp in sizeCtl.
	ResizeStampBits = 16

	// MaxResizers is the maximum number of threads that can help resize.
	MaxResizers = (1 << (32 - ResizeStampBits)) - 1

	// ResizeStampShift is the bit shift for recording size stamp in sizeCtl.
	ResizeStampShift = 32 - ResizeStampBits
)

// Constants needed for the implementation
const (
	MAXIMUM_CAPACITY = 1 << 30
	DEFAULT_CAPACITY = 16
	LOAD_FACTOR      = 0.75
	// The number of bits used for generation stamp in size_ctl
	RESIZE_STAMP_BITS = 16

	// Minimum number of rebinnings per transfer step
	MIN_TRANSFER_STRIDE = 16

	// The maximum number of threads that can help resize
	MAX_RESIZERS = (1 << (32 - RESIZE_STAMP_BITS)) - 1

	// The bit shift for recording size stamp in size_ctl
	RESIZE_STAMP_SHIFT = 32 - RESIZE_STAMP_BITS
)
