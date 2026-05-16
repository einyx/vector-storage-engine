//go:build vector

package vector

// searchResult is a candidate from a vector search: internal uint32 node ID
// and its distance from the query vector.
type searchResult struct {
	id       uint32
	distance float32
}

// searchMinHeap is a min-heap ordered by distance (smallest distance at root).
// All sift operations are inlined to avoid interface{} boxing.
type searchMinHeap struct {
	items []searchResult
	len   int
}

func (h *searchMinHeap) init(cap int) {
	h.len = 0
	if len(h.items) >= cap {
		return
	}
	h.items = make([]searchResult, cap)
}

func (h *searchMinHeap) push(item searchResult) {
	if h.len >= len(h.items) {
		h.items = append(h.items, item)
	} else {
		h.items[h.len] = item
	}
	h.len++
	// sift up
	i := h.len - 1
	for i > 0 {
		parent := (i - 1) >> 1
		if h.items[parent].distance <= h.items[i].distance {
			break
		}
		h.items[parent], h.items[i] = h.items[i], h.items[parent]
		i = parent
	}
}

func (h *searchMinHeap) pop() searchResult {
	top := h.items[0]
	h.len--
	if h.len > 0 {
		h.items[0] = h.items[h.len]
		// sift down
		i := 0
		for {
			left := (i << 1) + 1
			if left >= h.len {
				break
			}
			smallest := left
			if right := left + 1; right < h.len && h.items[right].distance < h.items[left].distance {
				smallest = right
			}
			if h.items[i].distance <= h.items[smallest].distance {
				break
			}
			h.items[i], h.items[smallest] = h.items[smallest], h.items[i]
			i = smallest
		}
	}
	return top
}

func (h *searchMinHeap) peek() searchResult {
	return h.items[0]
}

// searchMaxHeap is a max-heap ordered by distance (largest distance at root).
type searchMaxHeap struct {
	items []searchResult
	len   int
}

func (h *searchMaxHeap) init(cap int) {
	h.len = 0
	if len(h.items) >= cap {
		return
	}
	h.items = make([]searchResult, cap)
}

func (h *searchMaxHeap) push(item searchResult) {
	if h.len >= len(h.items) {
		h.items = append(h.items, item)
	} else {
		h.items[h.len] = item
	}
	h.len++
	// sift up
	i := h.len - 1
	for i > 0 {
		parent := (i - 1) >> 1
		if h.items[parent].distance >= h.items[i].distance {
			break
		}
		h.items[parent], h.items[i] = h.items[i], h.items[parent]
		i = parent
	}
}

func (h *searchMaxHeap) pop() searchResult {
	top := h.items[0]
	h.len--
	if h.len > 0 {
		h.items[0] = h.items[h.len]
		// sift down
		i := 0
		for {
			left := (i << 1) + 1
			if left >= h.len {
				break
			}
			largest := left
			if right := left + 1; right < h.len && h.items[right].distance > h.items[left].distance {
				largest = right
			}
			if h.items[i].distance >= h.items[largest].distance {
				break
			}
			h.items[i], h.items[largest] = h.items[largest], h.items[i]
			i = largest
		}
	}
	return top
}

func (h *searchMaxHeap) peek() searchResult {
	return h.items[0]
}

// boundedMaxHeap is a fixed-capacity max-heap for top-K collection.
// It keeps the K smallest distances seen so far by evicting the maximum
// when a smaller candidate arrives.
type boundedMaxHeap struct {
	items []searchResult
	len   int
	cap   int
}

func (h *boundedMaxHeap) init(capacity int) {
	h.len = 0
	h.cap = capacity
	if len(h.items) >= capacity {
		return
	}
	h.items = make([]searchResult, capacity)
}

// tryPush attempts to add item to the heap.
// When the heap is full, it only accepts items with distance < current max,
// replacing the max. Returns true if the item was accepted.
func (h *boundedMaxHeap) tryPush(item searchResult) bool {
	if h.len < h.cap {
		h.items[h.len] = item
		h.len++
		// sift up (max-heap)
		i := h.len - 1
		for i > 0 {
			parent := (i - 1) >> 1
			if h.items[parent].distance >= h.items[i].distance {
				break
			}
			h.items[parent], h.items[i] = h.items[i], h.items[parent]
			i = parent
		}
		return true
	}
	// heap full: only accept if item is closer than the current worst
	if item.distance >= h.items[0].distance {
		return false
	}
	// replace root and sift down
	h.items[0] = item
	i := 0
	for {
		left := (i << 1) + 1
		if left >= h.len {
			break
		}
		largest := left
		if right := left + 1; right < h.len && h.items[right].distance > h.items[left].distance {
			largest = right
		}
		if h.items[i].distance >= h.items[largest].distance {
			break
		}
		h.items[i], h.items[largest] = h.items[largest], h.items[i]
		i = largest
	}
	return true
}

func (h *boundedMaxHeap) peek() searchResult {
	return h.items[0]
}

// drainSorted returns all items sorted by distance ascending (smallest first)
// and resets the heap to empty. The returned slice is backed by the heap's
// internal array — callers must copy if they need it to outlive the next init.
func (h *boundedMaxHeap) drainSorted() []searchResult {
	n := h.len
	// pop repeatedly: each pop places the max at position h.len, which builds
	// the slice in descending order from the back. We then return a reversed view.
	for h.len > 0 {
		// swap root with last live element
		last := h.len - 1
		h.items[0], h.items[last] = h.items[last], h.items[0]
		h.len--
		// sift down the new root
		i := 0
		for {
			left := (i << 1) + 1
			if left >= h.len {
				break
			}
			largest := left
			if right := left + 1; right < h.len && h.items[right].distance > h.items[left].distance {
				largest = right
			}
			if h.items[i].distance >= h.items[largest].distance {
				break
			}
			h.items[i], h.items[largest] = h.items[largest], h.items[i]
			i = largest
		}
	}
	// heapsort on a max-heap produces ascending order naturally
	return h.items[:n]
}
