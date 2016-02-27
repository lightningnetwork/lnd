package btcdnotify

// confEntry...
type confEntry struct {
	*confirmationsNotification

	triggerHeight uint32
}

// confirmationHeap...
type confirmationHeap struct {
	items []*confEntry
}

func newConfirmationHeap() *confirmationHeap {
	var confItems []*confEntry
	return &confirmationHeap{confItems}
}

// Len returns the number of items in the priority queue. It is part of the
// heap.Interface implementation.
func (c *confirmationHeap) Len() int { return len(c.items) }

// Less returns whether the item in the priority queue with index i should sort
// before the item with index j. It is part of the heap.Interface implementation.
func (c *confirmationHeap) Less(i, j int) bool {
	return c.items[i].triggerHeight < c.items[j].triggerHeight
}

// Swap swaps the items at the passed indices in the priority queue. It is
// part of the heap.Interface implementation.
func (c *confirmationHeap) Swap(i, j int) {
	c.items[i], c.items[j] = c.items[j], c.items[i]
}

// Push pushes the passed item onto the priority queue. It is part of the
// heap.Interface implementation.
func (c *confirmationHeap) Push(x interface{}) {
	c.items = append(c.items, x.(*confEntry))
}

// Pop removes the highest priority item (according to Less) from the priority
// queue and returns it.  It is part of the heap.Interface implementation.
func (c *confirmationHeap) Pop() interface{} {
	n := len(c.items)
	x := c.items[n-1]
	c.items[n-1] = nil
	c.items = c.items[0 : n-1]
	return x
}
