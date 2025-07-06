// Copyright 2023-2024 antlabs. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package onebyone

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/antlabs/task/task/driver"
)

const (
	onebyoneDriverName = "onebyone"
)

func init() {
	driver.Register(onebyoneDriverName, &taskOneByOne{})
}

var _ driver.TaskDriver = (*taskOneByOne)(nil)
var _ driver.Tasker = (*taskOneByOne)(nil)
var _ driver.TaskExecutor = (*taskOneByOne)(nil)

type taskOneByOne struct {
	oneByOneChan chan func() bool
	sync.Once
	closed   uint32
	currency int64
	parent   *taskOneByOne
}

func (t *taskOneByOne) loop() {
	for cb := range t.oneByOneChan {
		cb()
	}
}

// 这里构造了一个新的实例
func (t *taskOneByOne) New(ctx context.Context, initCount, min, max int, c *driver.Conf) driver.Tasker {
	return t
}

// 创建一个执行器，由于没有node的概念，这里直接返回自己
func (t *taskOneByOne) NewExecutor() driver.TaskExecutor {
	var t2 taskOneByOne
	atomic.AddInt64(&t.currency, 1)
	t2.init()
	t2.parent = t
	return &t2
}

func (t *taskOneByOne) GetGoroutines() int {
	return int(atomic.LoadInt64(&t.currency))
} // 获取goroutine数

func (t *taskOneByOne) init() {
	t.oneByOneChan = make(chan func() bool, runtime.NumCPU())
	go t.loop()
}

func (t *taskOneByOne) AddTask(mu *sync.Mutex, f func() bool) (err error) {
	if atomic.LoadUint32(&t.closed) == 1 {
		return nil
	}

	defer func() {
		if e1 := recover(); e1 != nil {
			err = errors.New("found panic in AddTask: " + e1.(string))
			return
		}
	}()
	t.oneByOneChan <- f
	// TODO: 阻塞的情况如何处理?
	// greatws 处理overflow的fd
	return nil
}

func (t *taskOneByOne) Close(mu *sync.Mutex) error {
	t.Do(func() {
		close(t.oneByOneChan)
		atomic.AddInt64(&t.parent.currency, -1)
		atomic.StoreUint32(&t.closed, 1)
	})

	return nil
}
