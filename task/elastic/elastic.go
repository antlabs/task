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

package elastic

import (
	"context"
	"os"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/antlabs/task/cpu"
	"github.com/antlabs/task/task/driver"
)

const (
	elasticDriverName = "elastic"
)

func init() {
	driver.Register(elasticDriverName, &stream{})
}

var _ driver.TaskDriver = (*stream)(nil)
var _ driver.Tasker = (*stream)(nil)
var _ driver.TaskExecutor = (*streamExecutor)(nil)

type stream struct {
	initCount      int              // 初始go程数，如果是长驻go程有效果
	min            int              // 最小go程数
	max            int64            // 最大go程数
	goroutines     int32            // 当前go程数
	fn             chan func() bool // 数据
	haveData       chan struct{}    // 控制信号，表示有数据过来
	ctx            context.Context  // ctx
	conf           *driver.Conf     // 初始化传递过来的参数
	process        func()           // 处理单个任务的循环
	onMessageCount int64            //需要处理的OnMessage个数
}

func (s *stream) addOnMessageCount(n int) {
	atomic.AddInt64(&s.onMessageCount, int64(n))
}

func (s *stream) subOnMessageCount(n int) {
	atomic.AddInt64(&s.onMessageCount, int64(n))
}

func (s *stream) loadOnMessageCount() int64 {
	return atomic.LoadInt64(&s.onMessageCount)
}

func (s *stream) New(ctx context.Context, initCount, min, max int, c *driver.Conf) driver.Tasker {
	s2 := &stream{initCount: initCount,
		min:      min,
		max:      int64(max),
		fn:       make(chan func() bool, max),
		haveData: make(chan struct{}, max),
		ctx:      ctx,
		conf:     c,
	}

	// 默认长驻go程
	s2.process = s2.processLong
	if os.Getenv(envLoopKey) == envLoopShortValue {
		s2.process = s2.processShort
	}

	if runtime.GOOS == "darwin" {
		go s2.mainLoopOther()
	} else {
		go s2.mainLoopLinux()
	}
	return s2
}

func (s *stream) processLong() {
	for f := range s.fn {
		if f() {
			return
		}
	}
}

func (s *stream) processShort() {
	for {
		select {
		case f, ok := <-s.fn:
			if !ok {
				return
			}
			if f() {
				return
			}
		default:
			return
		}
	}
}

func (s *stream) addGoProcessNum(n int) {
	for i := 0; i < n; i++ {
		atomic.AddInt32(&s.goroutines, 1)
		go func() {
			defer atomic.AddInt32(&s.goroutines, -1)
			s.process()
		}()
	}
}

func (s *stream) lteInit() bool {
	return atomic.LoadInt32(&s.goroutines) <= int32(s.initCount)
}

func (s *stream) lteMax() bool {
	return atomic.LoadInt32(&s.goroutines) <= int32(s.max)
}

func (s *stream) mainLoopOther() {

	timeout := time.Second
	tm := time.NewTimer(timeout)
	subMax := 10
	subCount := 0
	for {
		select {
		case <-s.haveData:
			// <=10%时

			if s.lteMax() {
				s.addGoProcessNum(1)
			}

		case <-tm.C:
			subCount++
			if subCount == subMax {
				// 10s 没有数据过来，清一波go程
				currGo := atomic.LoadInt32(&s.goroutines)
				if currGo > int32(s.min) {
					need := int((float64(currGo) - float64(s.min)) * 0.1)
					for i := 0; i < need; i++ {
						s.fn <- func() (exit bool) {
							return true
						}
					}
				}
				subCount = 0
			}
			tm.Reset(timeout)

		}
	}
}

// 对于短暂任务来说，性能的最佳点是少量go程, 对于这种情况缓慢增加go程数，用于逼近性能最佳点
func (s *stream) mainLoopLinux() {
	timeout := time.Second
	tm := time.NewTimer(time.Hour * 24)

	addNum := int32(float64(s.max) * 0.05)

	subMax := 10
	subCount := 0

	initialInfo, _ := cpu.GetCPUInfo()
	initialProcInfo, _ := cpu.GetProcessCPUInfo(os.Getpid())
	notBusyProcess := false
	running := false
	for {
		select {
		case <-s.haveData:

			if s.lteInit() {
				s.addGoProcessNum(1)
			}

			if !running {
				tm.Reset(timeout)
				running = true
			}
		case <-tm.C:
			subCount++
			if subCount == subMax {

				// 10s 没有数据过来，清一波go程
				currGo := atomic.LoadInt32(&s.goroutines)
				if currGo > int32(s.min) && s.loadOnMessageCount() < 1000 {
					need := int((float64(currGo) - float64(s.min)) * 0.1)
					for i := 0; i < need; i++ {
						s.fn <- func() (exit bool) {
							return true
						}
					}
				}
				subCount = 0
			}

			currInfo, _ := cpu.GetCPUInfo()
			currProcInfo, _ := cpu.GetProcessCPUInfo(os.Getpid())

			notBusyProcess = cpu.CalculateProcessCPUPercent(initialProcInfo, currProcInfo, initialInfo, currInfo) < 0.7 // 0.7考虑到超线程, 超过这个值，后面能压榨的算力很少
			notBusyMachine := cpu.CalculateCPUPercent(initialInfo, currInfo) < 0.7                                      // 同上

			s.conf.Log.Debug("status",
				"notbusy", notBusyProcess,
				"letmax", s.lteMax(),
				"cpu-process", cpu.CalculateProcessCPUPercent(initialProcInfo, currProcInfo, initialInfo, currInfo),
				"haveData-len", len(s.haveData),
				"haveData-cap", cap(s.haveData),
				"cpu-machine", cpu.CalculateCPUPercent(initialInfo, currInfo),
				"onMessageCount", s.loadOnMessageCount(),
			)

			if notBusyMachine && notBusyProcess && s.lteMax() && s.loadOnMessageCount() > int64(s.GetGoroutines()) {
				s.addGoProcessNum(int(addNum))
			}

			initialInfo = currInfo
			initialProcInfo = currProcInfo

			tm.Reset(timeout)
		case <-s.ctx.Done():
			return
		}
	}
}

func (s *stream) NewExecutor() driver.TaskExecutor {
	return &streamExecutor{parent: s, list: make([]func() bool, 0, 4)}
}

func (s *stream) GetGoroutines() int {
	return int(atomic.LoadInt32(&s.goroutines))
}
