package cmap

// Constants related to table sizing, load factor, and resizing control,
// mirroring the values often found in concurrent hash map implementations
// like Java's ConcurrentHashMap and adapted from the Rust example.

const (
	// maximumCapacity is the largest possible table capacity.
	// This value is constrained to stay within reasonable allocation limits
	// and potentially aligns with control bit usage in fields like sizeCtl.
	// Must be a power of 2. (1 << 30)
	maximumCapacity = 1 << 30

	// defaultCapacity is the initial table capacity if not otherwise specified.
	// Must be a power of 2 >= 1 and <= maximumCapacity.
	defaultCapacity = 16

	// minTransferStride is the minimum number of bins that a thread processes
	// during a transfer step when resizing. Ranges are subdivided to allow
	// multiple resizer threads. This value serves as a lower bound to avoid
	// excessive contention. Should be at least defaultCapacity.
	minTransferStride = 16

	// resizeStampBits is the number of bits used for generation stamp in sizeCtl.
	// Affects how many concurrent resize operations can be distinguished.
	// Needs to be consistent with the size of the sizeCtl field (e.g., int64).
	// The value 16 is common, leaving bits for size count and resizer count.
	resizeStampBits = 16 // Assuming sizeCtl is 64-bit, this leaves plenty of room

	// maxResizers is the maximum number of threads that can help resize.
	// This value is derived from the number of bits available in sizeCtl
	// after accounting for the resize stamp. It's calculated based on a
	// 32-bit conceptual space as often used in the original Java implementation's
	// stamp/resizer count logic within sizeCtl.
	// (1 << (32 - resizeStampBits)) - 1
	maxResizers = (1 << (32 - resizeStampBits)) - 1

	// resizeStampShift is the bit shift for placing the resize stamp within sizeCtl.
	// Usually calculated relative to a 32-bit boundary for compatibility
	// with the original logic.
	// 32 - resizeStampBits
	resizeStampShift = 32 - resizeStampBits

	// ForwardingNode signifies a node forwarding to the next table (used in sizeCtl).
	// Typically a negative value. Using -1, similar to Java's MOVED.
	ForwardingNode int64 = -1

	// TreeBinNode signifies a node representing a tree bin (if implementing tree bins).
	// Typically a negative value. Using -2, similar to Java's TREEBIN.
	TreeBinNode int64 = -2 // Include if tree bins are planned

	// ReservationNode signifies a temporary reservation node (if used).
	// Typically a negative value. Using -3, similar to Java's RESERVED.
	ReservationNode int64 = -3 // Include if reservation nodes are planned
)

// loadFactor is the threshold determining when to resize.
// While the constant is defined, resizing decisions often use integer math
// like `n - (n >>> 2)` (equivalent to n * 0.75) to avoid floating point ops.
const loadFactor = 0.75
