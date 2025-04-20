// Package cmap implements a concurrent hash map similar to Java's ConcurrentHashMap
package cmap

import (
	"encoding/binary"
	"hash/fnv"
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"
)

// BinEntryType represents the type of bin entry
type BinEntryType int

const (
	NodeType BinEntryType = iota
	MovedType
)

// BinEntry is an entry in a hash bin
type BinEntry struct {
	entryType BinEntryType
	// For NodeType, this points to a Node
	// For MovedType, this points to the next table
	ptr unsafe.Pointer
}

// Node represents a key-value entry in the hash map
type Node struct {
	hash  uint64
	key   interface{}
	value atomic.Value
	next  atomic.Value // *Node
	mu    sync.Mutex   // Per-node mutex for updates
}

// Table represents the hash table with bins
type Table struct {
	bins []atomic.Value // []BinEntry
}

// Cmap is a concurrent hash map implementation
type Cmap struct {
	table         atomic.Value // *Table
	nextTable     atomic.Value // *Table
	transferIndex int64        // atomic
	count         uint64       // atomic
	sizeCtl       int64        // atomic
	hasher        func(interface{}) uint64
}

// NewCmap creates a new concurrent hash map
func New() *Cmap {
	return &Cmap{
		hasher:  defaultHasher,
		sizeCtl: int64(DefaultCapacity),
	}
}

// defaultHasher is the default hash function
func defaultHasher(key interface{}) uint64 {
	h := fnv.New64a()
	switch k := key.(type) {
	case string:
		h.Write([]byte(k))
	case []byte:
		h.Write(k)
	case int:
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], uint64(k))
		h.Write(buf[:])
	// Add more types as needed
	default:
		// For complex types, use reflection or require key to implement custom hashing
		panic("Unsupported key type")
	}
	return h.Sum64()
}

// hash computes the hash for a key
func (m *Cmap) hash(key interface{}) uint64 {
	return m.hasher(key)
}

// GetAndThen retrieves a value and applies a function to it if found
func (m *Cmap) GetAndThen(key interface{}, fn func(interface{}) interface{}) (interface{}, bool) {
	val, found := m.Get(key)
	if !found {
		return nil, false
	}
	return fn(val), true
}

// --- Previous definitions (BinEntryType, BinEntry, Node, Table, Cmap) assumed here ---
// ... (Use the definitions from the previous response) ...

// Get retrieves a value for the given key.
// This implementation mirrors the structure and atomic access patterns of the Rust
// version using crossbeam-epoch, relying on Go's atomics and garbage collector
// for memory safety instead of epoch-based reclamation guards.
func (m *Cmap) Get(key interface{}) (interface{}, bool) {
	// Calculate the hash for the key. Equivalent to `self.hash(key)`.
	h := m.hash(key)

	// Atomically load the current main table pointer.
	// Equivalent to `self.table.load(Ordering::SeqCst, guard)`.
	// Go's atomic Load ensures we get a valid pointer (or nil) without tearing.
	tableVal := m.table.Load()
	if tableVal == nil {
		// Equivalent to `table.is_null()`. Map is not initialized.
		return nil, false
	}

	// Cast the loaded value to the concrete table type.
	// Go's GC ensures 'table' remains valid as long as tableVal is referenced.
	// Equivalent to `unsafe { table.deref() }`.
	table := tableVal.(*Table)

	// Check if the table has any bins allocated.
	// Equivalent to `table.bins.len() == 0`.
	if len(table.bins) == 0 {
		return nil, false // Table initialized but empty.
	}

	// Find the node using the dedicated find function.
	// This function encapsulates the logic equivalent to:
	// - Calculating bin index: `table.bini(h)`
	// - Loading the bin entry: `table.bin(bini, guard)`
	// - Handling Moved entries and node chains: `bin.deref().find(h, key, guard)`
	// It uses atomic loads internally for safety.
	node := m.find(table, h, key) // Pass the specific table to search in

	// Check if the key was found.
	// Equivalent to `node.is_null()`.
	if node == nil {
		return nil, false
	}

	// If node is found, it's guaranteed to be a valid *Node pointer.
	// The Rust `node.as_node().unwrap()` is implicitly handled because
	// our `find` function directly returns `*Node` or `nil`.

	// Atomically load the value associated with the node.
	// Equivalent to `node.value.load(Ordering::SeqCst, guard)`.
	// node.value is an atomic.Value in Go.
	v := node.value.Load()

	// In Rust, `assert!(!v.is_null())` might exist if the map guarantees
	// a found node always has a non-null value pointer (even if the value
	// logically represents null). Go's `atomic.Value` can store `nil`.
	// We assume here that storing `nil` is valid. If `node != nil`, the key
	// exists, even if the associated value is `nil`.

	// Return the loaded value and true (indicating key was found).
	// Equivalent to `Some(v)`.
	return v, true
}

// WithCapacity creates a new map with the specified initial capacity
func WithCapacity(n int) *Cmap {
	if n <= 0 {
		panic("capacity must be positive")
	}
	m := New()

	// Calculate target size similar to Rust implementation
	size := int(float64(n)/LOAD_FACTOR) + 1

	// Find next power of two, capped at maximum capacity
	cap := nextPowerOfTwo(size)
	if cap > MAXIMUM_CAPACITY {
		cap = MAXIMUM_CAPACITY
	}

	m.sizeCtl = int64(cap)
	return m
}

// nextPowerOfTwo returns the next power of two greater than or equal to x
func nextPowerOfTwo(x int) int {
	if x <= 0 {
		return 1
	}
	if x >= MAXIMUM_CAPACITY {
		return MAXIMUM_CAPACITY
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

// --- Updated Search Logic ---

// findNodeInChain searches for a key within a specific node chain starting from headNode.
// This helper performs the iterative search through the linked list of nodes.
func findNodeInChain(headNode *Node, hash uint64, key interface{}) *Node {
	currentNode := headNode
	for currentNode != nil {
		// Check hash first for potential quick rejection
		if currentNode.hash == hash && currentNode.key == key {
			return currentNode // Found the node
		}
		// Move to the next node in the chain
		nextVal := currentNode.next.Load()
		if nextVal == nil {
			break // End of chain
		}
		currentNode = nextVal.(*Node)
	}
	// Key not found in this chain
	return nil
}

// find searches for a key starting from a specific bin in a specific table.
// It handles Moved entries iteratively and then searches the node chain.
// This function closely mirrors the logic flow of Rust's BinEntry::find.
func (m *Cmap) find(table *Table, hash uint64, key interface{}) *Node {
	currentTable := table // Start with the provided table

	for { // Loop specifically to handle Moved entries (table forwarding)
		if currentTable == nil || len(currentTable.bins) == 0 {
			return nil // Table is invalid or empty
		}

		// Calculate bin index in the *current* table
		bini := m.getBinIndex(hash, len(currentTable.bins))

		// Atomically load the BinEntry value from the table's bin
		binVal := currentTable.bins[bini].Load()
		if binVal == nil {
			// Important: A nil value here means the bin itself hasn't been initialized,
			// NOT that the key isn't present if the bin *was* initialized but empty.
			// An initialized but empty bin would have a NodeType BinEntry with a nil ptr.
			return nil // Bin is uninitialized or genuinely empty
		}

		// Type assert to get the actual BinEntry struct value
		bin := binVal.(BinEntry)

		// Process the loaded bin entry
		switch bin.entryType {
		case NodeType:
			// We've landed on a regular node bin (potentially empty).
			// Search the linked list starting from the node pointed to by bin.ptr.
			headNodePtr := bin.ptr
			if headNodePtr == nil {
				return nil // Bin is initialized but empty
			}
			// Delegate the chain search to the helper function
			return findNodeInChain((*Node)(headNodePtr), hash, key)

		case MovedType:
			// The bin has been moved. Follow the pointer to the next table.
			nextTablePtr := bin.ptr
			if nextTablePtr == nil {
				// This indicates a potential issue, a Moved entry should point somewhere.
				// Or it could mean the resize completed concurrently in a specific way.
				// Depending on the resize logic, returning nil might be appropriate.
				return nil // Or potentially retry from the top Cmap table? Needs context.
			}
			// Update currentTable to the forwarded table and continue the loop
			currentTable = (*Table)(nextTablePtr)
			continue // Go back to the start of the loop with the new table

		default:
			// Should theoretically not happen with defined constants
			return nil
		}
	}
	// The loop is only exited via returns within the switch cases.
}

// Non-recursive version of find for better Go performance (optional)
func (n *Node) findIterative(hash uint64, key interface{}) *Node {
	current := n
	for current != nil {
		if current.hash == hash && current.key == key {
			return current
		}

		nextVal := current.next.Load()
		if nextVal == nil {
			return nil
		}

		current = nextVal.(*Node)
	}

	return nil
}

// PutIfAbsent adds a key-value pair only if the key doesn't exist
func (m *Cmap) PutIfAbsent(key, value interface{}) (interface{}, bool) {
	return m.put(key, value, true)
}

// Put adds or updates a key-value pair
func (m *Cmap) Put(key, value interface{}) (interface{}, bool) {
	return m.put(key, value, false)
}

// put is the internal implementation for adding/updating entries
func (m *Cmap) put(key, value interface{}, onlyIfAbsent bool) (interface{}, bool) {
	// Calculate hash for the key
	hash := m.hash(key)

	// Main insertion loop
	for {
		// Get current table or initialize if needed
		tableVal := m.table.Load()
		var table *Table
		if tableVal == nil {
			table = m.initTable()
			continue
		} else {
			table = tableVal.(*Table)
			if len(table.bins) == 0 {
				table = m.initTable()
				continue
			}
		}

		// Calculate bin index
		bini := m.getBinIndex(hash, len(table.bins))
		binVal := table.bins[bini].Load()

		// Create a new node for insertion
		node := &Node{
			hash: hash,
			key:  key,
		}
		node.value.Store(value)
		nodeEntry := BinEntry{
			entryType: NodeType,
			ptr:       unsafe.Pointer(node),
		}

		// Fast path - bin is empty
		if binVal == nil {
			// Try to CAS our node into the empty bin
			if table.bins[bini].CompareAndSwap(nil, nodeEntry) {
				// Successful insertion, update count and return
				m.addCount(1, 0, false)
				return nil, true
			}
			// CAS failed, reload and try again
			continue
		}

		// Convert to BinEntry
		bin := binVal.(BinEntry)

		// Handle forwarding nodes (table is being resized)
		if bin.entryType == MovedType {
			nextTable := (*Table)(bin.ptr)
			m.helpTransfer(table, nextTable)
			continue // Retry with the new table
		}

		// At this point, we have a Node bin
		head := (*Node)(bin.ptr)

		// Quick check for first node match when replacement is disallowed
		if onlyIfAbsent && head.hash == hash && head.key == key {
			return head.value.Load(), false
		}

		// Need to take the bin lock to modify the chain
		head.mu.Lock()

		// Re-check that the head hasn't changed while we were waiting
		currentBinVal := table.bins[bini].Load()
		if currentBinVal != binVal {
			head.mu.Unlock()
			continue // Head changed, retry
		}

		// Now we "own" the bin
		binCount := 1
		current := head

		// Traverse the bin looking for our key
		for {
			// If we find the key, update or return based on onlyIfAbsent
			if current.hash == hash && current.key == key {
				oldVal := current.value.Load()
				if !onlyIfAbsent {
					// Update the value
					current.value.Store(value)
				}
				head.mu.Unlock()
				return oldVal, !onlyIfAbsent
			}

			// Check the next node
			nextVal := current.next.Load()
			if nextVal == nil {
				// We've reached the end - append our new node
				newNode := &Node{
					hash: hash,
					key:  key,
				}
				newNode.value.Store(value)
				current.next.Store(newNode)
				head.mu.Unlock()

				// Update the count and check for resize
				m.addCount(1, binCount, false)
				return nil, true
			}

			// Move to the next node
			current = nextVal.(*Node)
			binCount++
		}
	}
}

// getBinIndex calculates the bin index for a hash value
func (m *Cmap) getBinIndex(hash uint64, binCount int) int {
	return int(hash & uint64(binCount-1))
}

// initTable initializes the hash table if needed
func (m *Cmap) initTable() *Table {
	for {
		tableVal := m.table.Load()
		if tableVal != nil {
			table := tableVal.(*Table)
			if len(table.bins) > 0 {
				return table
			}
		}

		// Try to allocate the table
		sc := atomic.LoadInt64(&m.sizeCtl)
		if sc < 0 {
			// We lost the initialization race, yield and retry
			runtime.Gosched()
			continue
		}

		// Try to claim initialization responsibility
		if atomic.CompareAndSwapInt64(&m.sizeCtl, sc, -1) {
			// We get to initialize the table
			var table *Table
			tableVal = m.table.Load()

			if tableVal == nil || len(tableVal.(*Table).bins) == 0 {
				// Create a new table
				n := DEFAULT_CAPACITY
				if sc > 0 {
					n = int(sc)
				}

				table = &Table{
					bins: make([]atomic.Value, n),
				}
				m.table.Store(table)

				// Update size control to threshold (n - n/4)
				sc = int64(n) - int64(n>>2)
			} else {
				table = tableVal.(*Table)
			}

			// Reset size control
			atomic.StoreInt64(&m.sizeCtl, sc)
			return table
		}
	}
}

// addCount updates the counter and potentially triggers a resize
func (m *Cmap) addCount(n int64, binCount int, checkForResize bool) {
	// Update count
	var count uint64
	if n > 0 {
		count = atomic.AddUint64(&m.count, uint64(n))
	} else if n < 0 {
		absN := uint64(-n)
		count = atomic.AddUint64(&m.count, ^(absN - 1)) // Two's complement subtraction
	} else {
		count = atomic.LoadUint64(&m.count)
	}

	// If binCount is negative or resize check is disabled, skip resize check
	if binCount < 0 || !checkForResize {
		return
	}

	// Check if resize is needed
	for {
		sc := atomic.LoadInt64(&m.sizeCtl)
		if int64(count) < sc {
			// Not at the resize threshold yet
			break
		}

		tableVal := m.table.Load()
		if tableVal == nil {
			break
		}

		table := tableVal.(*Table)
		n := len(table.bins)
		if n >= MAXIMUM_CAPACITY {
			break
		}

		// Calculate resize stamp
		rs := resizeStamp(n) << RESIZE_STAMP_SHIFT

		if sc < 0 {
			// Resize already in progress
			if sc == rs+MAX_RESIZERS || sc == rs+1 {
				break
			}

			nextTableVal := m.nextTable.Load()
			if nextTableVal == nil {
				break
			}

			nextTable := nextTableVal.(*Table)

			if atomic.LoadInt64(&m.transferIndex) <= 0 {
				break
			}

			// Try to join the resize operation
			if atomic.CompareAndSwapInt64(&m.sizeCtl, sc, sc+1) {
				m.transfer(table, nextTable)
			}
		} else if atomic.CompareAndSwapInt64(&m.sizeCtl, sc, rs+2) {
			// Start a new resize operation
			m.transfer(table, nil)
		}

		// Reload count for next iteration
		count = atomic.LoadUint64(&m.count)
	}
}

// helpTransfer helps with a table transfer
func (m *Cmap) helpTransfer(table *Table, nextTable *Table) *Table {
	if table == nil || nextTable == nil {
		return table
	}

	// Calculate resize stamp
	rs := resizeStamp(len(table.bins)) << RESIZE_STAMP_SHIFT

	for {
		// Check if conditions are still valid
		nextTableVal := m.nextTable.Load()
		if nextTableVal == nil || nextTableVal.(*Table) != nextTable {
			break
		}

		tableVal := m.table.Load()
		if tableVal == nil || tableVal.(*Table) != table {
			break
		}

		sc := atomic.LoadInt64(&m.sizeCtl)
		if sc >= 0 ||
			sc == rs+MAX_RESIZERS ||
			sc == rs+1 ||
			atomic.LoadInt64(&m.transferIndex) <= 0 {
			break
		}

		// Try to participate in the transfer
		if atomic.CompareAndSwapInt64(&m.sizeCtl, sc, sc+1) {
			m.transfer(table, nextTable)
			break
		}
	}

	return nextTable
}

// transfer handles moving entries to the new table during resize
func (m *Cmap) transfer(table *Table, nextTable *Table) {
	n := len(table.bins)
	stride := MIN_TRANSFER_STRIDE

	// Initialize nextTable if needed
	if nextTable == nil {
		// Create a new table with double the size
		nextTable = &Table{
			bins: make([]atomic.Value, n<<1),
		}
		m.nextTable.Store(nextTable)
		atomic.StoreInt64(&m.transferIndex, int64(n))
	}

	nextN := len(nextTable.bins)
	advance := true
	finishing := false
	var i, bound int

	for {
		// Try to claim a range of bins
		for advance {
			i--
			if i >= bound || finishing {
				advance = false
				break
			}

			nextIndex := atomic.LoadInt64(&m.transferIndex)
			if nextIndex <= 0 {
				i = -1
				advance = false
				break
			}

			nextBound := int64(0)
			if nextIndex > int64(stride) {
				nextBound = nextIndex - int64(stride)
			}

			if atomic.CompareAndSwapInt64(&m.transferIndex, nextIndex, nextBound) {
				bound = int(nextBound)
				i = int(nextIndex) - 1
				advance = false
				break
			}
		}

		if i < 0 || i >= n || i+n >= nextN {
			// This section handles the completion of the transfer

			if finishing {
				// This thread is finishing the entire transfer
				// Clear nextTable reference
				m.nextTable.Store(nil)

				// Update table reference
				m.table.Store(nextTable)

				// Update size control threshold for next resize
				// (n << 1) - (n >> 1) = n*2 - n/2 = n*1.5
				atomic.StoreInt64(&m.sizeCtl, int64((n<<1)-(n>>1)))
				return
			}

			// Decrement the number of threads participating in transfer
			sc := atomic.LoadInt64(&m.sizeCtl)
			if atomic.CompareAndSwapInt64(&m.sizeCtl, sc, sc-1) {
				// Check if this thread should finish the transfer
				if (sc - 2) != resizeStamp(n)<<RESIZE_STAMP_SHIFT {
					return
				}

				// This thread is responsible for finishing the transfer
				finishing = true
				advance = true
				i = n // Start from the end
			}
			continue
		}

		// Process the bin at index i
		binVal := table.bins[i].Load()
		if binVal == nil {
			// Bin is empty, mark it as moved
			movedBin := BinEntry{
				entryType: MovedType,
				ptr:       unsafe.Pointer(nextTable),
			}

			if table.bins[i].CompareAndSwap(nil, movedBin) {
				advance = true
			}
			continue
		}

		bin := binVal.(BinEntry)
		if bin.entryType == MovedType {
			// Already processed
			advance = true
			continue
		}

		// Process a regular node bin
		head := (*Node)(bin.ptr)
		head.mu.Lock()

		// Re-check that the bin hasn't changed
		currentBinVal := table.bins[i].Load()
		if currentBinVal != binVal {
			head.mu.Unlock()
			continue
		}

		// Split the nodes based on their hash and the expanded table size
		runBit := (head.hash & uint64(n)) != 0
		lastRun := head
		p := head

		// Find the last run of nodes with the same destination
		for {
			nextVal := p.next.Load()
			if nextVal == nil {
				break
			}

			next := nextVal.(*Node)
			b := (next.hash & uint64(n)) != 0

			if b != runBit {
				runBit = b
				lastRun = p
			}

			p = next
		}

		// Prepare low and high bins
		var lowBin, highBin *Node
		if !runBit {
			lowBin = lastRun
		} else {
			highBin = lastRun
		}

		// Process nodes before the last run
		p = head
		for p != lastRun {
			nextVal := p.next.Load()
			next := (*Node)(nil)
			if nextVal != nil {
				next = nextVal.(*Node)
			}

			bit := (p.hash & uint64(n)) != 0

			// Create a new node
			newNode := &Node{
				hash: p.hash,
				key:  p.key,
			}
			newNode.value.Store(p.value.Load())

			// Add to appropriate bin
			if bit {
				newNode.next.Store(highBin)
				highBin = newNode
			} else {
				newNode.next.Store(lowBin)
				lowBin = newNode
			}

			p = next
		}

		// Store the bins in the next table
		if lowBin != nil {
			nextTable.bins[i].Store(BinEntry{
				entryType: NodeType,
				ptr:       unsafe.Pointer(lowBin),
			})
		}

		if highBin != nil {
			nextTable.bins[i+n].Store(BinEntry{
				entryType: NodeType,
				ptr:       unsafe.Pointer(highBin),
			})
		}

		// Mark the old bin as moved
		table.bins[i].Store(BinEntry{
			entryType: MovedType,
			ptr:       unsafe.Pointer(nextTable),
		})

		head.mu.Unlock()
		advance = true
	}
}

// Size returns the current number of elements in the map
func (m *Cmap) Size() int {
	return int(atomic.LoadUint64(&m.count))
}

// Clear removes all entries from the map
func (m *Cmap) Clear() {
	// Reset to initial state
	tableVal := m.table.Load()
	if tableVal == nil {
		return
	}

	// Create a new empty table
	table := &Table{
		bins: make([]atomic.Value, DEFAULT_CAPACITY),
	}

	// Reset all fields
	m.table.Store(table)
	m.nextTable.Store(nil)
	atomic.StoreInt64(&m.transferIndex, 0)
	atomic.StoreUint64(&m.count, 0)
	atomic.StoreInt64(&m.sizeCtl, int64(DEFAULT_CAPACITY-(DEFAULT_CAPACITY>>2)))
}

// resizeStamp returns the stamp bits for resizing a table of size n
func resizeStamp(n int) int64 {
	return int64(numberOfLeadingZeros(uint32(n)) | (1 << (RESIZE_STAMP_BITS - 1)))
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
