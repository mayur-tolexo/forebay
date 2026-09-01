package fasttier

import "container/list"

// firstReads remembers block identities that have been read once.
//
// A block that was not admitted leaves nothing behind, so recognising a second
// read needs a record of first ones. It holds no data and is bounded, because
// it is sized by the read stream rather than by the cache.
//
// The bound is what decides whether admission fires at all. Training reads the
// same shards each epoch, so the second read arrives a whole epoch after the
// first, and a record too small to span that gap sees every read as a first
// one and admits nothing. How large it should be is a measurement RFC-0018
// owns.
type firstReads struct {
	limit int
	order *list.List // Oldest at the front.
	seen  map[Key]*list.Element
}

func newFirstReads(limit int) *firstReads {
	return &firstReads{limit: limit, order: list.New(), seen: map[Key]*list.Element{}}
}

// sawBefore records a read and reports whether this key had been read before.
//
// Recording and answering are one operation because they always happen
// together, and splitting them invites a caller that answers without recording.
func (f *firstReads) sawBefore(k Key) bool {
	if _, ok := f.seen[k]; ok {
		return true
	}
	if f.limit <= 0 {
		return false
	}
	if f.order.Len() >= f.limit {
		oldest := f.order.Front()
		delete(f.seen, oldest.Value.(Key))
		f.order.Remove(oldest)
	}
	f.seen[k] = f.order.PushBack(k)
	return false
}

// forget drops a key, used when the block becomes resident and the record of
// its first read has done its job.
func (f *firstReads) forget(k Key) {
	if e, ok := f.seen[k]; ok {
		f.order.Remove(e)
		delete(f.seen, k)
	}
}

// len reports how many identities are held, for tests and for sizing.
func (f *firstReads) len() int { return f.order.Len() }
