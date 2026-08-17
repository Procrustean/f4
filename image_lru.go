package main

// imageLRU is a least-recently-used cache bounded by the weight of its
// values. A nil weight makes every entry weigh one, so the limit counts
// entries; otherwise the limit is a byte budget. Pinned entries are never
// evicted. Every method must be called with the pipeline lock held.
type imageLRU[K comparable, V any] struct {
	entries map[K]V
	order   []K
	size    int64
	limit   int64
	weight  func(V) int64
	pinned  map[K]bool
}

func newImageLRU[K comparable, V any](limit int64, weight func(V) int64) *imageLRU[K, V] {
	return &imageLRU[K, V]{entries: make(map[K]V), limit: limit, weight: weight}
}

// pin keeps an entry from being evicted; the caller replaces the old pin.
func (l *imageLRU[K, V]) pin(key K) {
	if l.pinned == nil {
		l.pinned = make(map[K]bool)
	}
	l.pinned[key] = true
}

func (l *imageLRU[K, V]) unpin(key K) {
	delete(l.pinned, key)
}

// get returns the value and moves it to the young end of the order.
func (l *imageLRU[K, V]) get(key K) (V, bool) {
	v, ok := l.entries[key]
	if ok {
		l.touch(key)
	}
	return v, ok
}

// peek returns the value without touching it.
func (l *imageLRU[K, V]) peek(key K) (V, bool) {
	v, ok := l.entries[key]
	return v, ok
}

// put stores v under key, replacing any previous value, and evicts the
// least recently used unpinned entries while over the budget. The newest
// entry always survives, so the picture on screen is kept even when it alone
// is larger than the whole budget.
func (l *imageLRU[K, V]) put(key K, v V) {
	wasPinned := l.pinned[key]
	l.delete(key)
	l.entries[key] = v
	l.order = append(l.order, key)
	l.size += l.cost(v)
	if wasPinned {
		l.pin(key)
	}
	for l.size > l.limit && len(l.order) > 1 {
		i := l.oldestUnpinned()
		if i < 0 {
			break
		}
		l.delete(l.order[i])
	}
}

// oldestUnpinned is the least recently used entry that is not pinned, or -1.
func (l *imageLRU[K, V]) oldestUnpinned() int {
	for i, k := range l.order {
		if !l.pinned[k] {
			return i
		}
	}
	return -1
}

func (l *imageLRU[K, V]) delete(key K) {
	v, ok := l.entries[key]
	if !ok {
		return
	}
	delete(l.entries, key)
	delete(l.pinned, key)
	l.size -= l.cost(v)
	for i, k := range l.order {
		if k == key {
			l.order = append(l.order[:i], l.order[i+1:]...)
			break
		}
	}
}

func (l *imageLRU[K, V]) touch(key K) {
	for i, k := range l.order {
		if k == key {
			l.order = append(l.order[:i], l.order[i+1:]...)
			break
		}
	}
	l.order = append(l.order, key)
}

func (l *imageLRU[K, V]) clear() {
	l.entries = make(map[K]V)
	l.order = nil
	l.size = 0
	l.pinned = nil
}

func (l *imageLRU[K, V]) cost(v V) int64 {
	if l.weight == nil {
		return 1
	}
	return l.weight(v)
}

func (l *imageLRU[K, V]) len() int     { return len(l.entries) }
func (l *imageLRU[K, V]) total() int64 { return l.size }
