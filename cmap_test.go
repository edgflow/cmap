package cmap

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestFlurryHashMap_Concurrent(t *testing.T) {
	// Create a new concurrent map
	f := New()

	// Number of concurrent goroutines
	numGoroutines := 100
	numOperations := 1000

	// Use a WaitGroup to ensure all goroutines complete
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	// Start time for benchmarking
	startTime := time.Now()

	// Launch goroutines
	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer wg.Done()

			// Each goroutine performs multiple operations
			for j := 0; j < numOperations; j++ {
				key := fmt.Sprintf("key-%d-%d", id, j)
				value := j

				// Mix of read and write operations
				if j%3 == 0 {
					// Put operation
					f.Put(key, value)
				} else if j%3 == 1 {
					// Get operation
					val, _ := f.Get(key)
					// Don't assert here to avoid contention on the test itself
					_ = val
				} else {
					// Update operation (get, modify, put)
					existingVal, _ := f.Get(key)
					if existingVal != nil {
						if intVal, ok := existingVal.(int); ok {
							f.Put(key, intVal+1)
						}
					} else {
						f.Put(key, 1)
					}
				}
			}
		}(i)
	}

	// Wait for all goroutines to complete
	wg.Wait()

	// Calculate execution time
	duration := time.Since(startTime)

	// Perform some basic validation
	totalOperations := numGoroutines * numOperations
	opsPerSecond := float64(totalOperations) / duration.Seconds()

	// Log performance metrics
	t.Logf("Completed %d operations in %v", totalOperations, duration)
	t.Logf("Operations per second: %.2f", opsPerSecond)

	// Verify the map integrity with a sample key
	f.Put("test-key", 42)
	val, ok := f.Get("test-key")
	if !ok {
		t.Errorf("Expected value 42 for key 'test-key', got %v", val)
	}
	if val != 42 {
		t.Errorf("Expected value 42 for key 'test-key', got %v", val)
	}
}
