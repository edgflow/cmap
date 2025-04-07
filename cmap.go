// Package flurry implements a concurrent hash map similar to Java's ConcurrentHashMap
package cmap

import (
	"encoding/binary"
	"hash/fnv"
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

// FlurryHashMap is a concurrent hash map implementation
type FlurryHashMap struct {
	table         atomic.Value // *Table
	nextTable     atomic.Value // *Table
	transferIndex int64        // atomic
	count         uint64       // atomic
	sizeCtl       int64        // atomic
	hasher        func(interface{}) uint64
}

// NewFlurryHashMap creates a new concurrent hash map
func NewFlurryHashMap() *FlurryHashMap {
	return &FlurryHashMap{
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
func (m *FlurryHashMap) hash(key interface{}) uint64 {
	return m.hasher(key)
}

// Get retrieves a value for the given key
func (m *FlurryHashMap) Get(key interface{}) (interface{}, bool) {
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

// findInBin searches for a key in a bin
func (m *FlurryHashMap) findInBin(bin *BinEntry, hash uint64, key interface{}) *Node {
	switch bin.entryType {
	case NodeType:
		node := (*Node)(bin.ptr)
		return node.find(hash, key)
	case MovedType:
		// If the bin has been moved, we need to follow the forwarding pointer
		nextTable := (*Table)(bin.ptr)
		// Get the bin in the next table
		bini := m.getBinIndex(hash, len(nextTable.bins))
		binVal := nextTable.bins[bini].Load()
		if binVal == nil {
			return nil
		}
		nextBin := binVal.(BinEntry)
		return m.findInBin(&nextBin, hash, key)
	}
	return nil
}

// find searches for a key in a node chain
func (n *Node) find(hash uint64, key interface{}) *Node {
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
func (m *FlurryHashMap) Put(key, value interface{}) (interface{}, bool) {
	return m.put(key, value, false)
}

// PutIfAbsent adds a key-value pair only if the key doesn't exist
func (m *FlurryHashMap) PutIfAbsent(key, value interface{}) (interface{}, bool) {
	return m.put(key, value, true)
}

// put is the internal implementation for adding/updating entries
func (m *FlurryHashMap) put(key, value interface{}, onlyIfAbsent bool) (interface{}, bool) {
	h := m.hash(key)

	var table *Table
	var created bool

	for {
		tableVal := m.table.Load()
		if tableVal == nil {
			// Initialize the table if it doesn't exist
			table = m.initTable()
			continue
		}

		table = tableVal.(*Table)
		if len(table.bins) == 0 {
			table = m.initTable()
			continue
		}

		bini := m.getBinIndex(h, len(table.bins))
		binVal := table.bins[bini].Load()

		if binVal == nil {
			// Fast path - bin is empty
			node := &Node{
				hash: h,
				key:  key,
			}
			node.value.Store(value)

			bin := BinEntry{
				entryType: NodeType,
				ptr:       unsafe.Pointer(node),
			}

			table.bins[bini].CompareAndSwap(nil, bin)
			// Even if CAS fails, we simply retry the loop
			continue
		}

		bin := binVal.(BinEntry)

		if bin.entryType == MovedType {
			// Help with the transfer and retry
			m.helpTransfer(table, bin.ptr)
			continue
		}

		// Slow path - bin is non-empty
		node := (*Node)(bin.ptr)
		node.mu.Lock()

		// Recheck that the bin hasn't changed
		currentBinVal := table.bins[bini].Load()
		if currentBinVal != binVal {
			node.mu.Unlock()
			continue
		}

		// Now we "own" the bin
		//var oldValue interface{}
		binCount := 1
		current := node

		// Check if key already exists in the bin
		for {
			if current.hash == h && current.key == key {
				oldVal := current.value.Load()
				if onlyIfAbsent {
					node.mu.Unlock()
					return oldVal, false
				}
				current.value.Store(value)
				node.mu.Unlock()
				return oldVal, false
			}

			nextVal := current.next.Load()
			if nextVal == nil {
				// End of the chain, append new node
				newNode := &Node{
					hash: h,
					key:  key,
				}
				newNode.value.Store(value)
				current.next.Store(newNode)
				created = true
				break
			}

			current = nextVal.(*Node)
			binCount++
		}

		node.mu.Unlock()

		if created {
			m.addCount(1, binCount)
		}

		return nil, created
	}
}

// getBinIndex calculates the bin index for a hash value
func (m *FlurryHashMap) getBinIndex(hash uint64, binCount int) int {
	return int(hash & uint64(binCount-1))
}

// initTable initializes the table
func (m *FlurryHashMap) initTable() *Table {
	for {
		sizeCtl := atomic.LoadInt64(&m.sizeCtl)
		if sizeCtl < 0 {
			// Someone else is initializing
			// Yield to allow them to progress
			continue
		}

		if atomic.CompareAndSwapInt64(&m.sizeCtl, sizeCtl, -1) {
			// We are now responsible for initialization
			tableVal := m.table.Load()
			if tableVal != nil {
				// Another thread beat us to it
				atomic.StoreInt64(&m.sizeCtl, sizeCtl)
				return tableVal.(*Table)
			}

			capacity := DefaultCapacity
			if sizeCtl > 0 {
				capacity = int(sizeCtl)
			}

			table := &Table{
				bins: make([]atomic.Value, capacity),
			}
			m.table.Store(table)

			// Update size control threshold
			atomic.StoreInt64(&m.sizeCtl, int64(float64(capacity)*LoadFactor))
			return table
		}
	}
}

// addCount adds to the count and triggers a resize if needed
func (m *FlurryHashMap) addCount(delta int64, binCount int) {
	// Add to count
	if delta != 0 {
		if delta > 0 {
			atomic.AddUint64(&m.count, uint64(delta))
		} else {
			atomic.AddUint64(&m.count, ^uint64(-delta-1))
		}
	}

	// Check if resize is needed
	if binCount == 0 {
		return
	}

	for {
		sizeCtl := atomic.LoadInt64(&m.sizeCtl)
		currentCount := atomic.LoadUint64(&m.count)

		if int64(currentCount) < sizeCtl {
			// Not at resize threshold yet
			break
		}

		tableVal := m.table.Load()
		if tableVal == nil {
			// Table will be initialized by another thread
			break
		}

		table := tableVal.(*Table)
		n := len(table.bins)

		if n >= MaximumCapacity {
			break
		}

		rs := resizeStamp(n) << ResizeStampShift

		if sizeCtl < 0 {
			// Ongoing resize
			if sizeCtl == rs+int64(MaxResizers) || sizeCtl == rs+1 {
				break
			}

			nextTableVal := m.nextTable.Load()
			if nextTableVal == nil {
				break
			}

			if atomic.LoadInt64(&m.transferIndex) <= 0 {
				break
			}

			// Try to join the transfer
			if atomic.CompareAndSwapInt64(&m.sizeCtl, sizeCtl, sizeCtl+1) {
				m.transfer(table, nextTableVal.(*Table))
			}
		} else if atomic.CompareAndSwapInt64(&m.sizeCtl, sizeCtl, rs+2) {
			// Initiate resize
			m.transfer(table, nil)
		}

		// Another resize might be needed
	}
}

// helpTransfer helps with a table transfer
func (m *FlurryHashMap) helpTransfer(table *Table, ptr unsafe.Pointer) {
	nextTable := (*Table)(ptr)
	m.transfer(table, nextTable)
}

// transfer handles moving entries to the new table during resize
func (m *FlurryHashMap) transfer(table *Table, nextTable *Table) {
	n := len(table.bins)
	stride := MinTransferStride

	if nextTable == nil {
		// Create a new table with double the size
		nextTable = &Table{
			bins: make([]atomic.Value, n<<1),
		}
		m.nextTable.Store(nextTable)
		atomic.StoreInt64(&m.transferIndex, int64(n))
	}

	nextN := len(nextTable.bins)
	advancing := true
	finishing := false
	var i, bound int

	for {
		// Try to claim a range of bins to transfer
		for advancing {
			i--
			if i >= bound || finishing {
				advancing = false
				break
			}

			nextIndex := atomic.LoadInt64(&m.transferIndex)
			if nextIndex <= 0 {
				i = -1
				advancing = false
				break
			}

			nextBound := int64(0)
			if nextIndex > int64(stride) {
				nextBound = nextIndex - int64(stride)
			}

			if atomic.CompareAndSwapInt64(&m.transferIndex, nextIndex, nextBound) {
				bound = int(nextBound)
				i = int(nextIndex)
				advancing = false
				break
			}
		}

		if i < 0 || i >= n || i+n >= nextN {
			// The resize has finished
			if finishing {
				// This branch is only taken by one thread
				m.nextTable.Store(nil)
				m.table.Store(nextTable)
				atomic.StoreInt64(&m.sizeCtl, int64((n<<1)-(n>>1)))
				return
			}

			sc := atomic.LoadInt64(&m.sizeCtl)
			if atomic.CompareAndSwapInt64(&m.sizeCtl, sc, sc-1) {
				if (sc - 2) != resizeStamp(n)<<ResizeStampShift {
					return
				}

				finishing = true
				advancing = true
				i = n
			}
			continue
		}

		binVal := table.bins[i].Load()
		if binVal == nil {
			// Bin is empty, mark it as moved
			movedBin := BinEntry{
				entryType: MovedType,
				ptr:       unsafe.Pointer(nextTable),
			}

			if table.bins[i].CompareAndSwap(nil, movedBin) {
				advancing = true
			}
			continue
		}

		bin := binVal.(BinEntry)

		if bin.entryType == MovedType {
			// Already processed
			advancing = true
			continue
		}

		// Process a node bin
		node := (*Node)(bin.ptr)
		node.mu.Lock()

		// Recheck that this is still the head
		currentBinVal := table.bins[i].Load()
		if currentBinVal != binVal {
			node.mu.Unlock()
			continue
		}

		// Split nodes into low (same index) and high (index + n) bins
		runBit := (node.hash & uint64(n)) != 0

		// Find the last run of nodes with same destination
		lastRun := node
		var p *Node
		for p = node; ; {
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

		// Clone nodes up to the last run
		var lowBin, highBin *Node
		if !runBit {
			lowBin = lastRun
		} else {
			highBin = lastRun
		}

		// Clone nodes that don't match the last run
		for p = node; p != lastRun; {
			nextVal := p.next.Load()
			var next *Node
			if nextVal != nil {
				next = nextVal.(*Node)
			}

			pRunBit := (p.hash & uint64(n)) != 0
			var newNode = &Node{
				hash: p.hash,
				key:  p.key,
			}
			newNode.value.Store(p.value.Load())

			if pRunBit {
				// Add to high bin
				if highBin == nil {
					highBin = newNode
				} else {
					newNode.next.Store(highBin)
					highBin = newNode
				}
			} else {
				// Add to low bin
				if lowBin == nil {
					lowBin = newNode
				} else {
					newNode.next.Store(lowBin)
					lowBin = newNode
				}
			}

			p = next
		}

		// Store bins in new table
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

		// Mark old bin as moved
		movedBin := BinEntry{
			entryType: MovedType,
			ptr:       unsafe.Pointer(nextTable),
		}
		table.bins[i].Store(movedBin)

		node.mu.Unlock()
		advancing = true
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
