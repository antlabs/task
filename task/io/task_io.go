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

package io

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/antlabs/task/task/driver"
)

const (
	ioDriverName = "io"
)

func init() {
	driver.Register(ioDriverName, &taskIO{})
}

var _ driver.TaskDriver = (*taskIO)(nil)
var _ driver.Tasker = (*taskIO)(nil)
var _ driver.TaskExecutor = (*taskIO)(nil)

type taskIO struct {
	closed    uint32
	executors int64 // 执行器数量计数
}

// 这里构造了一个新的实例
func (t *taskIO) New(ctx context.Context, initCount, min, max int, c *driver.Conf) driver.Tasker {
	return &taskIO{}
}

// 创建一个执行器，io模式下直接返回自己，因为不需要独立的go程
func (t *taskIO) NewExecutor() driver.TaskExecutor {
	atomic.AddInt64(&t.executors, 1)
	return &taskIO{
		executors: t.executors,
	}
}

// 获取goroutine数，io模式下始终为0，因为不创建新的go程
func (t *taskIO) GetGoroutines() int {
	return 0
}

// 添加任务，io模式下直接在当前go程中同步执行
func (t *taskIO) AddTask(mu *sync.Mutex, f func() bool) error {
	if atomic.LoadUint32(&t.closed) == 1 {
		return nil
	}

	// 直接在当前go程中执行任务，不使用channel或创建新go程
	f()
	return nil
}

// 关闭执行器
func (t *taskIO) Close(mu *sync.Mutex) error {
	atomic.StoreUint32(&t.closed, 1)
	return nil
}
