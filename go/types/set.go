// Copyright 2016 Attic Labs, Inc. All rights reserved.
// Licensed under the Apache License, version 2.0:
// http://www.apache.org/licenses/LICENSE-2.0

package types

import (
	"fmt"
	"sort"

	"github.com/attic-labs/noms/go/d"
	"github.com/attic-labs/noms/go/hash"
)

type Set struct {
	seq orderedSequence
	h   *hash.Hash
}

func newSet(seq orderedSequence) Set {
	return Set{seq, &hash.Hash{}}
}

func NewSet(v ...Value) Set {
	data := buildSetData(v)
	ch := newEmptySetSequenceChunker(nil, nil)

	for _, v := range data {
		ch.Append(v)
	}

	return newSet(ch.Done().(orderedSequence))
}

// NewStreamingSet takes an input channel of values and returns a output
// channel that will produce a finished Set. Values that are sent to the input
// channel must be in Noms sortorder, adding values to the input channel
// out of order will result in a panic. Once the input channel is closed
// by the caller, a finished Set will be sent to the output channel. See
// graph_builder.go for building collections with values that are not in order.
func NewStreamingSet(vrw ValueReadWriter, vals <-chan Value) <-chan Set {
	outChan := make(chan Set, 1)
	go func() {
		defer close(outChan)
		ch := newEmptySetSequenceChunker(vrw, vrw)
		var lastV Value
		for v := range vals {
			d.PanicIfTrue(v == nil)
			if lastV != nil {
				d.PanicIfFalse(lastV == nil || lastV.Less(v))
			}
			ch.Append(v)
		}
		outChan <- newSet(ch.Done().(orderedSequence))
	}()
	return outChan
}

// Diff computes the diff from |last| to |m| using the top-down algorithm,
// which completes as fast as possible while taking longer to return early
// results than left-to-right.
func (s Set) Diff(last Set, changes chan<- ValueChanged, closeChan <-chan struct{}) {
	if s.Equals(last) {
		return
	}
	orderedSequenceDiffTopDown(last.seq, s.seq, changes, closeChan)
}

// DiffHybrid computes the diff from |last| to |s| using a hybrid algorithm
// which balances returning results early vs completing quickly, if possible.
func (s Set) DiffHybrid(last Set, changes chan<- ValueChanged, closeChan <-chan struct{}) {
	if s.Equals(last) {
		return
	}
	orderedSequenceDiffBest(last.seq, s.seq, changes, closeChan)
}

// DiffLeftRight computes the diff from |last| to |s| using a left-to-right
// streaming approach, optimised for returning results early, but not
// completing quickly.
func (s Set) DiffLeftRight(last Set, changes chan<- ValueChanged, closeChan <-chan struct{}) {
	if s.Equals(last) {
		return
	}
	orderedSequenceDiffLeftRight(last.seq, s.seq, changes, closeChan)
}

// Collection interface
func (s Set) Len() uint64 {
	return s.seq.numLeaves()
}

func (s Set) Empty() bool {
	return s.Len() == 0
}

func (s Set) sequence() sequence {
	return s.seq
}

func (s Set) hashPointer() *hash.Hash {
	return s.h
}

// Value interface
func (s Set) Value(vrw ValueReadWriter) Value {
	return s
}

func (s Set) Equals(other Value) bool {
	return s.Hash() == other.Hash()
}

func (s Set) Less(other Value) bool {
	return valueLess(s, other)
}

func (s Set) Hash() hash.Hash {
	if s.h.IsEmpty() {
		*s.h = getHash(s)
	}

	return *s.h
}

func (s Set) WalkValues(cb ValueCallback) {
	s.IterAll(func(v Value) {
		cb(v)
	})
}

func (s Set) WalkRefs(cb RefCallback) {
	s.seq.WalkRefs(cb)
}

func (s Set) typeOf() *Type {
	return s.seq.typeOf()
}

func (s Set) Kind() NomsKind {
	return SetKind
}

func (s Set) First() Value {
	cur := newCursorAt(s.seq, emptyKey, false, false, false)
	if !cur.valid() {
		return nil
	}
	return cur.current().(Value)
}

func (s Set) At(idx uint64) Value {
	if idx >= s.Len() {
		panic(fmt.Errorf("Out of bounds: %d >= %d", idx, s.Len()))
	}

	cur := newCursorAtIndex(s.seq, idx, false)
	return cur.current().(Value)
}

func (s Set) getCursorAtValue(v Value, readAhead bool) (cur *sequenceCursor, found bool) {
	cur = newCursorAtValue(s.seq, v, true, false, readAhead)
	found = cur.idx < cur.seq.seqLen() && cur.current().(Value).Equals(v)
	return
}

func (s Set) Has(v Value) bool {
	cur := newCursorAtValue(s.seq, v, false, false, false)
	return cur.valid() && cur.current().(Value).Equals(v)
}

type setIterCallback func(v Value) bool

func (s Set) Iter(cb setIterCallback) {
	cur := newCursorAt(s.seq, emptyKey, false, false, false)
	cur.iter(func(v interface{}) bool {
		return cb(v.(Value))
	})
}

type setIterAllCallback func(v Value)

func (s Set) IterAll(cb setIterAllCallback) {
	cur := newCursorAt(s.seq, emptyKey, false, false, true)
	cur.iter(func(v interface{}) bool {
		cb(v.(Value))
		return false
	})
}

func (s Set) Iterator() SetIterator {
	return s.IteratorAt(0)
}

func (s Set) IteratorAt(idx uint64) SetIterator {
	return &setIterator{
		cursor: newCursorAtIndex(s.seq, idx, false),
		s:      s,
	}
}

func (s Set) IteratorFrom(val Value) SetIterator {
	return &setIterator{
		cursor: newCursorAtValue(s.seq, val, false, false, false),
		s:      s,
	}
}

func (s Set) Edit() *SetEditor {
	return NewSetEditor(s)
}

func buildSetData(values ValueSlice) ValueSlice {
	if len(values) == 0 {
		return ValueSlice{}
	}

	uniqueSorted := make(ValueSlice, 0, len(values))
	sort.Stable(values)
	last := values[0]
	for i := 1; i < len(values); i++ {
		v := values[i]
		if !v.Equals(last) {
			uniqueSorted = append(uniqueSorted, last)
		}
		last = v
	}

	return append(uniqueSorted, last)
}

func makeSetLeafChunkFn(vr ValueReader) makeChunkFn {
	return func(level uint64, items []sequenceItem) (Collection, orderedKey, uint64) {
		d.PanicIfFalse(level == 0)
		setData := make([]Value, len(items), len(items))

		var lastValue Value
		for i, item := range items {
			v := item.(Value)
			d.PanicIfFalse(lastValue == nil || lastValue.Less(v))
			lastValue = v
			setData[i] = v
		}

		set := newSet(newSetLeafSequence(vr, setData...))
		var key orderedKey
		if len(setData) > 0 {
			key = newOrderedKey(setData[len(setData)-1])
		}

		return set, key, uint64(len(items))
	}
}

func newEmptySetSequenceChunker(vr ValueReader, vw ValueWriter) *sequenceChunker {
	return newEmptySequenceChunker(vr, vw, makeSetLeafChunkFn(vr), newOrderedMetaSequenceChunkFn(SetKind, vr), hashValueBytes)
}
