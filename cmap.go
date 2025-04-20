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
func NewCmap() *Cmap {
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

// Get retrieves a value for the given key
func (m *Cmap) Get(key interface{}) (interface{}, bool) {
	h := m.hash(key)

	// Load the current table
	tableVal := m.table.Load()
	if tableVal == nil {
		return nil, false
	}

	table := tableVal.(*Table)
	if len(table.bins) == 0 {
		return nil, false
	}

	bini := m.getBinIndex(h, len(table.bins))
	binVal := table.bins[bini].Load()
	if binVal == nil {
		return nil, false
	}

	bin := binVal.(BinEntry)
	node := m.findInBin(&bin, h, key)
	if node == nil {
		return nil, false
	}

	v := node.value.Load()
	if v == nil {
		return nil, false
	}

	return v, true
}

// findInBin searches for a key in a bin, following the same pattern as the Rust code
func (m *Cmap) findInBin(bin *BinEntry, hash uint64, key interface{}) *Node {
	switch bin.entryType {
	case NodeType:
		// We're in a regular node bin
		node := (*Node)(bin.ptr)
		return node.find(hash, key)

	case MovedType:
		// The bin has been moved, we need to follow the pointer to the next table
		nextTable := (*Table)(bin.ptr)

		// Loop to handle potential multiple forwards (matching Rust's loop approach)
		for {
			if nextTable == nil {
				return nil
			}

			// Get the bin index in the next table
			bini := m.getBinIndex(hash, len(nextTable.bins))

			// Load the bin
			binVal := nextTable.bins[bini].Load()
			if binVal == nil {
				return nil
			}

			// Convert to BinEntry
			nextBin := binVal.(BinEntry)

			// Based on the type, either find in this bin or follow another forward pointer
			switch nextBin.entryType {
			case NodeType:
				// Found a node bin, search in it
				return m.findInBin(&nextBin, hash, key)

			case MovedType:
				// Another forward, continue the loop
				nextTable = (*Table)(nextBin.ptr)
				continue
			}
		}
	}

	return nil
}

// find searches for a key in a node chain using a recursive approach
// to match the Rust implementation more closely
func (n *Node) find(hash uint64, key interface{}) *Node {
	// Check if this is the node we're looking for
	if n.hash == hash && n.key == key {
		return n
	}

	// Check the next node
	nextVal := n.next.Load()
	if nextVal == nil {
		return nil
	}

	// Recursively check the next node
	return nextVal.(*Node).find(hash, key)
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

// Put adds or updates a key-value pair
func (m *Cmap) Put(key, value interface{}) (interface{}, bool) {
	return m.put(key, value, false)
}

// PutIfAbsent adds a key-value pair only if the key doesn't exist
func (m *Cmap) PutIfAbsent(key, value interface{}) (interface{}, bool) {
	return m.put(key, value, true)
}

// put is the internal implementation for adding/updating entries
func (m *Cmap) put(key, value interface{}, onlyIfAbsent bool) (interface{}, bool) {
	// Calculate hash for the key
	hash := m.hasher(key)

	// Create a new node to insert (we'll reuse this if insertion fails)
	node := &Node{
		hash: hash,
		key:  key,
	}
	node.value.Store(value)
	nodeEntry := BinEntry{
		entryType: NodeType,
		ptr:       unsafe.Pointer(node),
	}

	// Main insertion loop
	for {
		// Get current table or initialize if needed
		tableVal := m.table.Load()
		var table *Table
		if tableVal == nil || tableVal.(*Table).bins == nil || len(tableVal.(*Table).bins) == 0 {
			table = m.initTable()
			continue
		} else {
			table = tableVal.(*Table)
		}

		// Calculate bin index
		bini := int(hash & uint64(len(table.bins)-1))

		// Load the bin at the index
		binVal := table.bins[bini].Load()

		// Fast path - bin is empty
		if binVal == nil {
			// Try to CAS our node into the empty bin
			if table.bins[bini].CompareAndSwap(nil, nodeEntry) {
				// Successful insertion, update count and return
				m.addCount(1, 0)
				return nil, true
			}
			// CAS failed, reload the bin and try again
			binVal = table.bins[bini].Load()
			if binVal == nil {
				continue // Retry if bin is still empty
			}
		}

		// Convert to BinEntry
		bin := binVal.(BinEntry)

		// Handle forwarding nodes (the bin has been moved during resize)
		if bin.entryType == MovedType {
			//nextTable := (*Table)(bin.ptr)
			m.helpTransfer(table, bin.ptr)
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
				return oldVal, false
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

				// Update the count
				m.addCount(1, binCount)
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
			if table.bins != nil && len(table.bins) > 0 {
				return table
			}
		}

		// Try to allocate the table
		sc := atomic.LoadInt64(&m.sizeCtl)
		if sc < 0 {
			// We lost the initialization race, just yield and spin
			runtime.Gosched() // Similar to std::thread::yield_now()
			continue
		}

		// Try to claim initialization responsibility
		if atomic.CompareAndSwapInt64(&m.sizeCtl, sc, -1) {
			// We get to do it!
			tableVal = m.table.Load()
			var table *Table

			if tableVal == nil || tableVal.(*Table).bins == nil || len(tableVal.(*Table).bins) == 0 {
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
func (m *Cmap) addCount(n int64, binCount int) {
	// Update count based on the sign of n
	var count uint64
	if n > 0 {
		count = atomic.AddUint64(&m.count, uint64(n))
	} else if n < 0 {
		absN := uint64(-n)
		count = atomic.AddUint64(&m.count, ^(absN - 1)) // Equivalent to subtract in two's complement
	} else {
		count = atomic.LoadUint64(&m.count)
	}

	// If binCount is negative, the caller does not want us to consider a resize
	if binCount < 0 {
		return
	}

	// Consider resize
	for {
		sc := atomic.LoadInt64(&m.sizeCtl)
		if int64(count) < sc {
			// We're not at the next resize point yet
			break
		}

		// Get current table
		tableVal := m.table.Load()
		if tableVal == nil {
			// Table will be initialized by another thread
			break
		}

		table := tableVal.(*Table)
		n := len(table.bins)
		if n >= MAXIMUM_CAPACITY {
			// Can't resize any more
			break
		}

		// Calculate resize stamp
		rs := resizeStamp(n) << RESIZE_STAMP_SHIFT

		if sc < 0 {
			// Ongoing resize! Can we join?
			if sc == rs+MAX_RESIZERS || sc == rs+1 {
				break
			}

			// Check nextTable
			nextTableVal := m.nextTable.Load()
			if nextTableVal == nil {
				break
			}

			nextTable := nextTableVal.(*Table)

			// Check if there's still work to do
			if atomic.LoadInt64(&m.transferIndex) <= 0 {
				break
			}

			// Try to join the resize operation
			if atomic.CompareAndSwapInt64(&m.sizeCtl, sc, sc+1) {
				m.transfer(table, nextTable)
			}
		} else if atomic.CompareAndSwapInt64(&m.sizeCtl, sc, rs+2) {
			// A resize is needed but hasn't started yet
			// Start a new resize operation
			m.transfer(table, nil)
		}

		// Another resize may be needed
		count = atomic.LoadUint64(&m.count)
	}
}

// helpTransfer helps with a table transfer
func (m *Cmap) helpTransfer(table *Table, ptr unsafe.Pointer) *Table {
	nextTable := (*Table)(ptr)
	// helpTransfer helps with a table transfer for concurrent resizing operations.
	// It participates in the transfer when another thread is already expanding the table.
	// Early return if either table is nil/null
	if table == nil || nextTable == nil {
		return table
	}

	// Calculate resize stamp - identifies the current resize operation
	rs := resizeStamp(len(table.bins)) << ResizeStampShift

	// Try to help with transfer as long as conditions remain stable
	for atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&m.nextTable))) == unsafe.Pointer(nextTable) &&
		atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&m.table))) == unsafe.Pointer(table) {

		// Load current size control value atomically
		sc := atomic.LoadInt64(&m.sizeCtl)

		// Exit conditions - no help needed if:
		// 1. sc >= 0: resize operation completed
		// 2. sc == rs + MAX_RESIZERS: max helpers already working
		// 3. sc == rs + 1: resize operation finished but not yet reset
		// 4. transferIndex <= 0: no more bins to process
		if sc >= 0 ||
			sc == rs+MaxResizers ||
			sc == rs+1 ||
			atomic.LoadInt64(&m.transferIndex) <= 0 {
			break
		}

		// Try to increment helper count
		if atomic.CompareAndSwapInt64(&m.sizeCtl, sc, sc+1) {
			// Successfully registered as helper, do transfer work
			m.transfer(table, nextTable)
			break
		}
	}

	return nextTable

}

const (
	RESIZE_STAMP_BITS   = 16
	RESIZE_STAMP_SHIFT  = 16
	MIN_TRANSFER_STRIDE = 16
)

// transfer handles moving entries to the new table during resize
func (m *Cmap) transfer(table *Table, nextTable *Table) {
	n := len(table.bins)
	stride := MIN_TRANSFER_STRIDE

	// If nextTable is nil, we're initiating a resize
	if nextTable == nil {
		// Create a new table with double the size
		nextTable = &Table{
			bins: make([]atomic.Value, n<<1),
		}
		// Swap in the new next table
		m.nextTable.Store(nextTable)
		atomic.StoreInt64(&m.transferIndex, int64(n))
	}

	nextN := len(nextTable.bins)
	advance := true
	finishing := false
	var i, bound int

	for {
		// Try to claim a range of bins for transfer
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
				i = int(nextIndex) - 1 // Adjust to match the rust decrementation
				advance = false
				break
			}
		}

		if i < 0 || i >= n || i+n >= nextN {
			// The resize has finished

			if finishing {
				// This branch is only taken by one thread to finish the resize
				m.nextTable.Store(nil)
				m.table.Store(nextTable)
				// Set the new threshold for the next resize
				atomic.StoreInt64(&m.sizeCtl, int64((n<<1)-(n>>1)))
				return
			}

			sc := atomic.LoadInt64(&m.sizeCtl)
			if atomic.CompareAndSwapInt64(&m.sizeCtl, sc, sc-1) {
				// Check if we're the last worker thread
				if (sc - 2) != resizeStamp(n)<<RESIZE_STAMP_SHIFT {
					return
				}

				// We are the chosen thread to finish the resize
				finishing = true
				advance = true
				i = n // Start from the end as per the rust code
			}
			continue
		}

		// Get the bin at index i
		binVal := table.bins[i].Load()
		if binVal == nil {
			// Bin is empty, mark it as moved
			movedBin := BinEntry{
				entryType: MovedType,
				ptr:       unsafe.Pointer(nextTable),
			}

			// Try to CAS the bin
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

		// Must be a Node type
		node := (*Node)(bin.ptr)
		node.mu.Lock()

		// Recheck that this is still the head
		currentBinVal := table.bins[i].Load()
		if currentBinVal != binVal {
			node.mu.Unlock()
			continue
		}

		// Find the last run of nodes with same destination
		runBit := (node.hash & uint64(n)) != 0
		lastRun := node
		p := node

		// Traverse the linked list to find the last run
		for {
			nextVal := p.next.Load()
			if nextVal == nil {
				break
			}
			next := nextVal.(*Node)

			nextRunBit := (next.hash & uint64(n)) != 0
			if nextRunBit != runBit {
				runBit = nextRunBit
				lastRun = p
			}

			p = next
		}

		// Set up low and high bins
		var lowBin, highBin *Node
		if !runBit {
			// The last run belongs in the low bin
			lowBin = lastRun
		} else {
			// The last run belongs in the high bin
			highBin = lastRun
		}

		// Process all nodes before the last run
		// These need to be cloned since they might go to different bins
		p = node
		for p != lastRun {
			nextVal := p.next.Load()
			next := (*Node)(nil)
			if nextVal != nil {
				next = nextVal.(*Node)
			}

			pRunBit := (p.hash & uint64(n)) != 0

			// Create a new node with the same data
			newNode := &Node{
				hash: p.hash,
				key:  p.key,
				mu:   sync.Mutex{}, // New mutex
			}
			newNode.value.Store(p.value.Load())

			// Determine which bin to put it in
			if pRunBit {
				// To high bin
				newNode.next.Store(highBin)
				highBin = newNode
			} else {
				// To low bin
				newNode.next.Store(lowBin)
				lowBin = newNode
			}

			p = next
		}

		// Store the bins in the next table
		if lowBin != nil {
			lowBinEntry := BinEntry{
				entryType: NodeType,
				ptr:       unsafe.Pointer(lowBin),
			}
			nextTable.bins[i].Store(lowBinEntry)
		}

		if highBin != nil {
			highBinEntry := BinEntry{
				entryType: NodeType,
				ptr:       unsafe.Pointer(highBin),
			}
			nextTable.bins[i+n].Store(highBinEntry)
		}

		// Mark the old bin as moved
		movedBin := BinEntry{
			entryType: MovedType,
			ptr:       unsafe.Pointer(nextTable),
		}
		table.bins[i].Store(movedBin)

		node.mu.Unlock()
		advance = true
	}
}

// resizeStamp returns the stamp bits for resizing a table of size n
func resizeStamp(n int) int64 {
	return int64(numberOfLeadingZeros(uint32(n)) | (1 << (ResizeStampBits - 1)))
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
