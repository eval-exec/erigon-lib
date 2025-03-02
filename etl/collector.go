/*
   Copyright 2021 Erigon contributors

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package etl

import (
	"bytes"
	"container/heap"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/c2h5oh/datasize"
	"github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/ledgerwatch/log/v3"
	"github.com/ugorji/go/codec"
)

const TmpDirName = "etl-temp"

type LoadNextFunc func(originalK, k, v []byte) error
type LoadFunc func(k, v []byte, table CurrentTableReader, next LoadNextFunc) error

// Collector performs the job of ETL Transform, but can also be used without "E" (Extract) part
// as a Collect Transform Load
type Collector struct {
	extractNextFunc ExtractNextFunc
	flushBuffer     func([]byte, bool) error
	dataProviders   []dataProvider
	allFlushed      bool
	autoClean       bool
	noLogs          bool
	bufType         int
	logPrefix       string
}

// NewCollectorFromFiles creates collector from existing files (left over from previous unsuccessful loading)
func NewCollectorFromFiles(logPrefix, tmpdir string) (*Collector, error) {
	if _, err := os.Stat(tmpdir); os.IsNotExist(err) {
		return nil, nil
	}
	fileInfos, err := ioutil.ReadDir(tmpdir)
	if err != nil {
		return nil, fmt.Errorf("collector from files - reading directory %s: %w", tmpdir, err)
	}
	if len(fileInfos) == 0 {
		return nil, nil
	}
	dataProviders := make([]dataProvider, len(fileInfos))
	for i, fileInfo := range fileInfos {
		var dataProvider fileDataProvider
		dataProvider.file, err = os.Open(filepath.Join(tmpdir, fileInfo.Name()))
		if err != nil {
			return nil, fmt.Errorf("collector from files - opening file %s: %w", fileInfo.Name(), err)
		}
		dataProviders[i] = &dataProvider
	}
	return &Collector{dataProviders: dataProviders, allFlushed: true, autoClean: false, logPrefix: logPrefix}, nil
}

// NewCriticalCollector does not clean up temporary files if loading has failed
func NewCriticalCollector(logPrefix, tmpdir string, sortableBuffer Buffer) *Collector {
	c := NewCollector(logPrefix, tmpdir, sortableBuffer)
	c.autoClean = false
	return c
}

func NewCollector(logPrefix, tmpdir string, sortableBuffer Buffer) *Collector {
	c := &Collector{autoClean: true, bufType: getTypeByBuffer(sortableBuffer), logPrefix: logPrefix}
	encoder := codec.NewEncoder(nil, &cbor)

	c.flushBuffer = func(currentKey []byte, canStoreInRam bool) error {
		if sortableBuffer.Len() == 0 {
			return nil
		}
		var provider dataProvider
		var err error
		sortableBuffer.Sort()
		if canStoreInRam && len(c.dataProviders) == 0 {
			provider = KeepInRAM(sortableBuffer)
			c.allFlushed = true
		} else {
			doFsync := !c.autoClean /* is critical collector */
			provider, err = FlushToDisk(encoder, sortableBuffer, tmpdir, doFsync, c.noLogs)
		}
		if err != nil {
			return err
		}
		if provider != nil {
			c.dataProviders = append(c.dataProviders, provider)
		}
		return nil
	}

	c.extractNextFunc = func(originalK, k []byte, v []byte) error {
		sortableBuffer.Put(common.Copy(k), common.Copy(v))
		if sortableBuffer.CheckFlushSize() {
			if err := c.flushBuffer(originalK, false); err != nil {
				return err
			}
		}
		return nil
	}
	return c
}

func (c *Collector) Collect(k, v []byte) error {
	return c.extractNextFunc(k, k, v)
}

func (c *Collector) NoLogs(v bool) { c.noLogs = v }

func (c *Collector) Load(db kv.RwTx, toBucket string, loadFunc LoadFunc, args TransformArgs) error {
	defer func() {
		if c.autoClean {
			c.Close()
		}
	}()
	if !c.allFlushed {
		if e := c.flushBuffer(nil, true); e != nil {
			return e
		}
	}
	if err := loadFilesIntoBucket(c.logPrefix, db, toBucket, c.bufType, c.dataProviders, loadFunc, args); err != nil {
		return err
	}
	return nil
}

func (c *Collector) Close() {
	totalSize := uint64(0)
	for _, p := range c.dataProviders {
		totalSize += p.Dispose()
	}
	if totalSize > 0 {
		log.Info(fmt.Sprintf("[%s] etl: temp files removed", c.logPrefix), "total size", datasize.ByteSize(totalSize).HumanReadable())
	}
}

func loadFilesIntoBucket(logPrefix string, db kv.RwTx, bucket string, bufType int, providers []dataProvider, loadFunc LoadFunc, args TransformArgs) error {
	decoder := codec.NewDecoder(nil, &cbor)
	var m runtime.MemStats

	h := &Heap{comparator: args.Comparator}
	heap.Init(h)
	for i, provider := range providers {
		if key, value, err := provider.Next(decoder); err == nil {
			he := HeapElem{key, i, value}
			heap.Push(h, he)
		} else /* we must have at least one entry per file */ {
			eee := fmt.Errorf("%s: error reading first readers: n=%d current=%d provider=%s err=%w",
				logPrefix, len(providers), i, provider, err)
			panic(eee)
		}
	}
	var c kv.RwCursor

	currentTable := &currentTableReader{db, bucket}
	haveSortingGuaranties := isIdentityLoadFunc(loadFunc) // user-defined loadFunc may change ordering
	var lastKey []byte
	if bucket != "" { // passing empty bucket name is valid case for etl when DB modification is not expected
		var err error
		c, err = db.RwCursor(bucket)
		if err != nil {
			return err
		}
		var errLast error
		lastKey, _, errLast = c.Last()
		if errLast != nil {
			return errLast
		}
	}
	var canUseAppend bool
	isDupSort := kv.ChaindataTablesCfg[bucket].Flags&kv.DupSort != 0 && !kv.ChaindataTablesCfg[bucket].AutoDupSortKeysConversion

	logEvery := time.NewTicker(30 * time.Second)
	defer logEvery.Stop()

	i := 0
	var prevK []byte
	loadNextFunc := func(originalK, k, v []byte) error {
		if i == 0 {
			isEndOfBucket := lastKey == nil || bytes.Compare(lastKey, k) == -1
			canUseAppend = haveSortingGuaranties && isEndOfBucket
		}
		i++

		// SortableOldestAppearedBuffer must guarantee that only 1 oldest value of key will appear
		// but because size of buffer is limited - each flushed file does guarantee "oldest appeared"
		// property, but files may overlap. files are sorted, just skip repeated keys here
		if bufType == SortableOldestAppearedBuffer && bytes.Equal(prevK, k) {
			return nil
		}
		prevK = k

		select {
		default:
		case <-logEvery.C:
			logArs := []interface{}{"into", bucket}
			if args.LogDetailsLoad != nil {
				logArs = append(logArs, args.LogDetailsLoad(k, v)...)
			} else {
				logArs = append(logArs, "current key", makeCurrentKeyStr(k))
			}

			runtime.ReadMemStats(&m)
			logArs = append(logArs, "alloc", common.ByteCount(m.Alloc), "sys", common.ByteCount(m.Sys))
			log.Info(fmt.Sprintf("[%s] ETL [2/2] Loading", logPrefix), logArs...)
		}

		if canUseAppend && len(v) == 0 {
			return nil // nothing to delete after end of bucket
		}
		if len(v) == 0 {
			if err := c.Delete(k, nil); err != nil {
				return err
			}
			return nil
		}
		if canUseAppend {
			if isDupSort {
				if err := c.(kv.RwCursorDupSort).AppendDup(k, v); err != nil {
					return fmt.Errorf("%s: bucket: %s, appendDup: k=%x, %w", logPrefix, bucket, k, err)
				}
			} else {
				if err := c.Append(k, v); err != nil {
					return fmt.Errorf("%s: bucket: %s, append: k=%x, v=%x, %w", logPrefix, bucket, k, v, err)
				}
			}

			return nil
		}
		if err := c.Put(k, v); err != nil {
			return fmt.Errorf("%s: put: k=%x, %w", logPrefix, k, err)
		}
		return nil
	}
	// Main loading loop
	for h.Len() > 0 {
		if err := common.Stopped(args.Quit); err != nil {
			return err
		}

		element := (heap.Pop(h)).(HeapElem)
		provider := providers[element.TimeIdx]
		err := loadFunc(element.Key, element.Value, currentTable, loadNextFunc)
		if err != nil {
			return err
		}
		if element.Key, element.Value, err = provider.Next(decoder); err == nil {
			heap.Push(h, element)
		} else if err != io.EOF {
			return fmt.Errorf("%s: error while reading next element from disk: %w", logPrefix, err)
		}
	}

	runtime.ReadMemStats(&m)
	log.Trace(
		fmt.Sprintf("[%s] ETL Load done", logPrefix),
		"bucket", bucket,
		"records", i,
		"alloc", common.ByteCount(m.Alloc), "sys", common.ByteCount(m.Sys))

	return nil
}

func makeCurrentKeyStr(k []byte) string {
	var currentKeyStr string
	if k == nil {
		currentKeyStr = "final"
	} else if len(k) < 4 {
		currentKeyStr = fmt.Sprintf("%x", k)
	} else if k[0] == 0 && k[1] == 0 && k[2] == 0 && k[3] == 0 && len(k) >= 8 { // if key has leading zeroes, show a bit more info
		currentKeyStr = fmt.Sprintf("%x", k)
	} else {
		currentKeyStr = fmt.Sprintf("%x...", k[:4])
	}
	return currentKeyStr
}
