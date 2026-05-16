//go:build vector

package vector

import (
	"testing"
)

func TestSearchMinHeap_PushPop(t *testing.T) {
	var h searchMinHeap
	h.init(4)

	h.push(searchResult{id: 1, distance: 3.0})
	h.push(searchResult{id: 2, distance: 1.0})
	h.push(searchResult{id: 3, distance: 2.0})

	first := h.pop()
	second := h.pop()
	third := h.pop()

	if first.distance != 1.0 {
		t.Errorf("expected first pop distance 1.0, got %f", first.distance)
	}
	if second.distance != 2.0 {
		t.Errorf("expected second pop distance 2.0, got %f", second.distance)
	}
	if third.distance != 3.0 {
		t.Errorf("expected third pop distance 3.0, got %f", third.distance)
	}
}

func TestSearchMaxHeap_PushPop(t *testing.T) {
	var h searchMaxHeap
	h.init(4)

	h.push(searchResult{id: 1, distance: 1.0})
	h.push(searchResult{id: 2, distance: 3.0})
	h.push(searchResult{id: 3, distance: 2.0})

	top := h.pop()
	if top.distance != 3.0 {
		t.Errorf("expected pop to return largest distance 3.0, got %f", top.distance)
	}
}

func TestSearchMaxHeap_Peek(t *testing.T) {
	var h searchMaxHeap
	h.init(4)

	h.push(searchResult{id: 1, distance: 1.0})
	h.push(searchResult{id: 2, distance: 3.0})
	h.push(searchResult{id: 3, distance: 2.0})

	top := h.peek()
	if top.distance != 3.0 {
		t.Errorf("expected peek to return largest distance 3.0, got %f", top.distance)
	}
	if h.len != 3 {
		t.Errorf("peek should not remove element, expected len 3, got %d", h.len)
	}
}

func TestBoundedMaxHeap_TryPush(t *testing.T) {
	var h boundedMaxHeap
	h.init(3)

	h.tryPush(searchResult{id: 1, distance: 1.0})
	h.tryPush(searchResult{id: 2, distance: 2.0})
	h.tryPush(searchResult{id: 3, distance: 3.0})

	if h.len != 3 {
		t.Fatalf("expected len 3, got %d", h.len)
	}

	// push smaller item — should evict max (3.0) and be accepted
	accepted := h.tryPush(searchResult{id: 4, distance: 1.5})
	if !accepted {
		t.Error("expected smaller item to be accepted")
	}
	if h.len != 3 {
		t.Errorf("expected len still 3, got %d", h.len)
	}
	if h.peek().distance == 3.0 {
		t.Error("max (3.0) should have been evicted")
	}

	// push larger item — should be rejected since heap is full and new distance > current max
	maxDist := h.peek().distance
	accepted = h.tryPush(searchResult{id: 5, distance: maxDist + 1.0})
	if accepted {
		t.Error("expected larger item to be rejected")
	}
	if h.len != 3 {
		t.Errorf("expected len still 3, got %d", h.len)
	}
}

func TestBoundedMaxHeap_DrainSorted(t *testing.T) {
	var h boundedMaxHeap
	h.init(4)

	h.tryPush(searchResult{id: 1, distance: 3.0})
	h.tryPush(searchResult{id: 2, distance: 1.0})
	h.tryPush(searchResult{id: 3, distance: 4.0})
	h.tryPush(searchResult{id: 4, distance: 2.0})

	results := h.drainSorted()

	if len(results) != 4 {
		t.Fatalf("expected 4 results, got %d", len(results))
	}
	for i := 1; i < len(results); i++ {
		if results[i].distance < results[i-1].distance {
			t.Errorf("results not in ascending order at index %d: %f > %f", i, results[i-1].distance, results[i].distance)
		}
	}
	if h.len != 0 {
		t.Errorf("expected heap empty after drain, got len %d", h.len)
	}
}

func TestSearchMinHeap_Empty(t *testing.T) {
	var h searchMinHeap
	h.init(8)

	if h.len != 0 {
		t.Errorf("expected empty heap len 0, got %d", h.len)
	}
}
