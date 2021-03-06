//  Copyright (c) 2018 Couchbase, Inc.
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the
//  License. You may obtain a copy of the License at
//    http://www.apache.org/licenses/LICENSE-2.0
//  Unless required by applicable law or agreed to in writing,
//  software distributed under the License is distributed on an "AS
//  IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
//  express or implied. See the License for the specific language
//  governing permissions and limitations under the License.

package main

import (
	"fmt"
	"sync"

	"github.com/blevesearch/bleve/index/scorch"
	"github.com/couchbase/moss"

	log "github.com/couchbase/clog"
)

type sizeFunc func(interface{}) uint64

type appHerder struct {
	memQuota   uint64
	appQuota   uint64
	indexQuota uint64
	queryQuota uint64

	m        sync.Mutex
	waitCond *sync.Cond
	waiting  int

	indexes map[interface{}]sizeFunc

	// Tracks the amount of memory used by running queries
	runningQueryUsed uint64
}

func newAppHerder(memQuota uint64, appRatio, indexRatio,
	queryRatio float64) *appHerder {
	ah := &appHerder{
		memQuota: memQuota,
		indexes:  map[interface{}]sizeFunc{},
	}
	ah.appQuota = uint64(float64(ah.memQuota) * appRatio)
	ah.indexQuota = uint64(float64(ah.appQuota) * indexRatio)
	ah.queryQuota = uint64(float64(ah.appQuota) * queryRatio)
	ah.waitCond = sync.NewCond(&ah.m)
	log.Printf("app_herder: memQuota: %d, appQuota: %d, indexQutoa: %d, "+
		"queryQuota: %d", memQuota, ah.appQuota, ah.indexQuota, ah.queryQuota)
	return ah
}

// *** Indexing Callbacks

func (a *appHerder) onClose(c interface{}) {
	a.m.Lock()

	if a.waiting > 0 {
		log.Printf("app_herder: close progress, waiting: %d", a.waiting)
	}

	delete(a.indexes, c)

	a.m.Unlock()
}

func (a *appHerder) onBatchExecuteStart(c interface{}, s sizeFunc) {

	a.m.Lock()

	a.indexes[c] = s

	for a.overMemQuotaForIndexingLOCKED() {
		// If we're over the memory quota, then wait for persister progress.

		log.Printf("app_herder: waiting for more memory to be available")

		a.waiting++
		a.waitCond.Wait()
		a.waiting--

		log.Printf("app_herder: resuming upon memory reduction ..")
	}

	a.m.Unlock()
}

func (a *appHerder) indexingMemoryLOCKED() (rv uint64) {
	for index, indexSizeFunc := range a.indexes {
		rv += indexSizeFunc(index)
	}
	return
}

func (a *appHerder) overMemQuotaForIndexingLOCKED() bool {
	memUsed := a.indexingMemoryLOCKED()

	// first make sure indexing (on it's own) doesn't exceed the
	// index portion of the quota
	if memUsed > a.indexQuota {
		log.Printf("app_herder: indexing mem used %d over indexing quota %d",
			memUsed, a.indexQuota)
		return true
	}

	// second add in running queries and check combined app quota
	memUsed += a.runningQueryUsed
	if memUsed > a.appQuota {
		log.Printf("app_herder: indexing mem plus query %d now over app quota %d",
			memUsed, a.appQuota)
	}
	return memUsed > a.appQuota
}

func (a *appHerder) onPersisterProgress() {
	a.m.Lock()

	if a.waiting > 0 {
		log.Printf("app_herder: persistence progress, waiting: %d", a.waiting)
	}

	a.waitCond.Broadcast()

	a.m.Unlock()
}

// *** Query Interface

func (a *appHerder) StartQuery(size uint64) error {
	a.m.Lock()
	defer a.m.Unlock()
	memUsed := a.runningQueryUsed + size

	// first make sure querying (on it's own) doesn't exceed the
	// query portion of the quota
	if memUsed > a.queryQuota {
		return fmt.Errorf("app_herder: this query %d plus running queries: %d "+
			"would exceed query quota: %d",
			size, a.runningQueryUsed, a.queryQuota)
	}

	// second add in indexing and check combined app quota
	indexingMem := a.indexingMemoryLOCKED()
	memUsed += indexingMem
	if memUsed > a.appQuota {
		return fmt.Errorf("app_herder: this query %d plus running queries: %d "+
			"plus indexing: %d would exceed app quota: %d",
			size, a.runningQueryUsed, indexingMem, a.appQuota)
	}

	// record the addition
	a.runningQueryUsed += size
	return nil
}

func (a *appHerder) EndQuery(size uint64) {
	a.m.Lock()
	a.runningQueryUsed -= size

	if a.waiting > 0 {
		log.Printf("app_herder: query ended, waiting: %d", a.waiting)
	}

	a.waitCond.Broadcast()

	a.m.Unlock()
}

// *** Moss Wrapper

func (a *appHerder) MossHerderOnEvent() func(moss.Event) {
	return func(event moss.Event) { a.onMossEvent(event) }
}

func mossSize(c interface{}) uint64 {
	s, err := c.(moss.Collection).Stats()
	if err != nil {
		log.Warnf("app_herder: moss stats, err: %v", err)
		return 0
	}
	return s.CurDirtyBytes
}

func (a *appHerder) onMossEvent(event moss.Event) {
	if event.Collection.Options().LowerLevelUpdate == nil {
		return
	}
	switch event.Kind {
	case moss.EventKindClose:
		a.onClose(event.Collection)

	case moss.EventKindBatchExecuteStart:
		a.onBatchExecuteStart(event.Collection, mossSize)

	case moss.EventKindPersisterProgress:
		a.onPersisterProgress()

	default:
		return
	}
}

// *** Scorch Wrapper
func (a *appHerder) ScorchHerderOnEvent() func(scorch.Event) {
	return func(event scorch.Event) { a.onScorchEvent(event) }
}

func scorchSize(s interface{}) uint64 {
	return s.(*scorch.Scorch).MemoryUsed()
}

func (a *appHerder) onScorchEvent(event scorch.Event) {
	switch event.Kind {
	case scorch.EventKindClose:
		a.onClose(event.Scorch)

	case scorch.EventKindBatchIntroductionStart:
		a.onBatchExecuteStart(event.Scorch, scorchSize)

	case scorch.EventKindPersisterProgress:
		a.onPersisterProgress()

	default:
		return
	}
}
