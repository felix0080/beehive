package raftstore

import (
	"sync"

	"github.com/fagongzi/goetty"
)

var (
	bufPool              sync.Pool
	asyncApplyResultPool sync.Pool
)

func acquireBuf() *goetty.ByteBuf {
	value := bufPool.Get()
	if value == nil {
		return goetty.NewByteBuf(64)
	}

	buf := value.(*goetty.ByteBuf)
	buf.Resume(64)

	return buf
}

func releaseBuf(value *goetty.ByteBuf) {
	value.Clear()
	value.Release()
	bufPool.Put(value)
}

func acquireAsyncApplyResult() *asyncApplyResult {
	v := asyncApplyResultPool.Get()
	if v == nil {
		return &asyncApplyResult{}
	}

	return v.(*asyncApplyResult)
}

func releaseAsyncApplyResult(res *asyncApplyResult) {
	res.reset()
	asyncApplyResultPool.Put(res)
}
