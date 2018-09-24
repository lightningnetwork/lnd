package htlcswitch

import (
  "time"
)

// This is the node class for the priority queue
// Note that the underlying data structure used is a min heap
type node struct {
  priority time.Time
  packet *htlcPacket
}

// Smaller the priority value, more the priority
func makeNode(pkt *htlcPacket) node {
  p := time.Now()
  
  return node {
    priority  : p,
    packet    : pkt,
  }
}

// priorityQueue type implements the heap.Interface
// To be used with heap module
type priorityQueue []node

// sort.Interface Less function
// Note that priority is a comparator interface
func (p priorityQueue) Less(i, j int) bool {
  return p[i].priority.Before(p[j].priority)
}

// sort.Interface Len function
func (p priorityQueue) Len() int {
  return len(p)
}

// sort.Interface Swap function
func (p priorityQueue) Swap(i, j int) {
  p[i], p[j] = p[j], p[i]
}

// heap.Interface Push function
func (p *priorityQueue) Push(x interface{}) {
  *p = append(*p, x.(node))
}

// heap.Interface Pop function
func (p *priorityQueue) Pop() interface{} {
  t := *p
  ret := t[len(t)-1]
  *p = t[:len(t)-1]
  return ret
}
