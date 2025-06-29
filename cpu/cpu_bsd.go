//go:build darwin || freebsd

package cpu

import (
	"context"
	"errors"
	"fmt"
	"math"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
)

// 使用 go-purego 库加载动态库

// CPU时间统计结构体
type TimesStat struct {
	CPU       string  // CPU 标识符
	User      float64 // 用户模式时间
	System    float64 // 系统模式时间
	Idle      float64 // 空闲时间
	Nice      float64 // 优先级调整时间
	Iowait    float64 // IO 等待时间
	Irq       float64 // 硬中断时间
	Softirq   float64 // 软中断时间
	Steal     float64 // 虚拟化环境中的空闲时间
	Guest     float64 // 运行虚拟 CPU 的时间
	GuestNice float64 // 运行低优先级虚拟 CPU 的时间
}

// 计算总时间
func (t *TimesStat) Total() float64 {
	return t.User + t.System + t.Idle + t.Nice + t.Iowait + t.Irq + t.Softirq + t.Steal + t.Guest + t.GuestNice
}

// 用于跟踪上次 CPU 使用率的计算
type lastPercent struct {
	sync.Mutex
	lastCPUTimes    []TimesStat
	lastPerCPUTimes []TimesStat
}

var lastCPUPercent lastPercent

// 进程结构体
type Process struct {
	Pid          int32
	lastCPUTimes *TimesStat
	lastCPUTime  time.Time
}

// 系统时钟频率，默认128Hz
var clocksPerSec = float64(128)

// 动态库结构体
type Library struct {
	handle uintptr
	path   string
}

// Mach 常量
const (
	KERN_SUCCESS = 0

	// CPU 状态
	CPU_STATE_USER   = 0
	CPU_STATE_SYSTEM = 1
	CPU_STATE_IDLE   = 2
	CPU_STATE_NICE   = 3

	// Mach 主机信息
	HOST_CPU_LOAD_INFO       = 3
	HOST_CPU_LOAD_INFO_COUNT = 4

	// 进程信息
	PROC_PIDTASKINFO = 4
)

// CPU 负载信息结构体
type hostCpuLoadInfoData struct {
	cpuTicks [4]uint32
}

// 进程任务信息结构体
type procTaskInfo struct {
	Virtual_size      uint64
	Resident_size     uint64
	Total_user        uint64
	Total_system      uint64
	Threads_user      uint64
	Threads_system    uint64
	Policy            int32
	Faults            int32
	Pageins           int32
	Cow_faults        int32
	Messages_sent     int32
	Messages_received int32
	Syscalls_mach     int32
	Syscalls_unix     int32
	Csw               int32
	Threadnum         int32
	Numrunning        int32
	Priority          int32
}

// 初始化函数
func init() {
	// 获取系统时钟频率
	var libPath string
	if runtime.GOOS == "darwin" {
		libPath = "/usr/lib/libSystem.B.dylib"
	} else {
		// FreeBSD
		libPath = "/lib/libc.so.7"
	}

	lib, err := loadLibrary(libPath)
	if err != nil {
		return
	}
	defer lib.Close()

	// 获取 sysctl 函数
	sysctlFunc, err := lib.GetSymbol("sysctl")
	if err != nil {
		return
	}

	// 调用 sysctl 获取时钟频率
	var clockrate struct {
		Hz int32
	}
	mib := []int32{6, 7} // CTL_KERN, KERN_CLOCKRATE
	size := uint64(unsafe.Sizeof(clockrate))

	status, _, _ := purego.SyscallN(sysctlFunc,
		uintptr(unsafe.Pointer(&mib[0])),
		uintptr(2),
		uintptr(unsafe.Pointer(&clockrate)),
		uintptr(unsafe.Pointer(&size)),
		0, 0)

	if status == 0 && clockrate.Hz > 0 {
		clocksPerSec = float64(clockrate.Hz)
	}
}

// 加载动态库
func loadLibrary(path string) (*Library, error) {
	handle, err := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return nil, fmt.Errorf("failed to load library %s: %v", path, err)
	}

	return &Library{
		handle: handle,
		path:   path,
	}, nil
}

// 获取符号
func (l *Library) GetSymbol(name string) (uintptr, error) {
	sym, err := purego.Dlsym(l.handle, name)
	if err != nil {
		return 0, fmt.Errorf("failed to find symbol %s: %v", name, err)
	}
	return sym, nil
}

// 关闭动态库
func (l *Library) Close() {
	purego.Dlclose(l.handle)
}

// 获取本机CPU使用率
func PercentWithContext(ctx context.Context, interval time.Duration, percpu bool) ([]float64, error) {
	if interval <= 0 {
		return percentUsedFromLastCallWithContext(ctx, percpu)
	}

	// 获取第一次测量
	first, err := TimesWithContext(ctx, percpu)
	if err != nil {
		return nil, err
	}

	// 等待指定时间间隔
	select {
	case <-time.After(interval):
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	// 获取第二次测量
	second, err := TimesWithContext(ctx, percpu)
	if err != nil {
		return nil, err
	}

	// 计算使用率
	return calculateAllBusy(first, second)
}

func percentUsedFromLastCallWithContext(ctx context.Context, percpu bool) ([]float64, error) {
	lastCPUPercent.Lock()
	defer lastCPUPercent.Unlock()

	var cpuTimes []TimesStat
	if percpu {
		cpuTimes = lastCPUPercent.lastPerCPUTimes
	} else {
		cpuTimes = lastCPUPercent.lastCPUTimes
	}

	if cpuTimes == nil {
		// 如果没有上次的数据，获取当前数据并返回0
		current, err := TimesWithContext(ctx, percpu)
		if err != nil {
			return nil, err
		}

		if percpu {
			lastCPUPercent.lastPerCPUTimes = current
		} else {
			lastCPUPercent.lastCPUTimes = current
		}

		return make([]float64, len(current)), nil
	}

	// 获取当前数据
	current, err := TimesWithContext(ctx, percpu)
	if err != nil {
		return nil, err
	}

	// 计算使用率
	ret, err := calculateAllBusy(cpuTimes, current)
	if err != nil {
		return ret, err
	}

	// 更新上次的数据
	if percpu {
		lastCPUPercent.lastPerCPUTimes = current
	} else {
		lastCPUPercent.lastCPUTimes = current
	}

	return ret, nil
}

func calculateAllBusy(t1, t2 []TimesStat) ([]float64, error) {
	if len(t1) != len(t2) {
		return nil, errors.New("CPU times arrays have different lengths")
	}

	ret := make([]float64, len(t1))
	for i := 0; i < len(t1); i++ {
		ret[i] = calculateBusy(t1[i], t2[i])
	}
	return ret, nil
}

func calculateBusy(t1, t2 TimesStat) float64 {
	t1All, t1Busy := getAllBusy(t1)
	t2All, t2Busy := getAllBusy(t2)

	if t2All-t1All == 0 {
		return 0
	}

	return math.Min(100, math.Max(0, (t2Busy-t1Busy)/(t2All-t1All)*100))
}

func getAllBusy(t TimesStat) (float64, float64) {
	busy := t.User + t.System + t.Nice + t.Iowait + t.Irq + t.Softirq + t.Steal + t.Guest + t.GuestNice
	return t.Total(), busy
}

func TimesWithContext(ctx context.Context, percpu bool) ([]TimesStat, error) {
	var libPath string
	if runtime.GOOS == "darwin" {
		libPath = "/usr/lib/libSystem.B.dylib"
	} else {
		// FreeBSD
		libPath = "/lib/libc.so.7"
	}

	lib, err := loadLibrary(libPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load library: %v", err)
	}
	defer lib.Close()

	if percpu {
		return perCPUTimes(lib)
	}
	return allCPUTimes(lib)
}

func allCPUTimes(lib *Library) ([]TimesStat, error) {
	machHostSelfFunc, err := lib.GetSymbol("mach_host_self")
	if err != nil {
		return nil, fmt.Errorf("failed to get mach_host_self: %v", err)
	}

	hostStatisticsFunc, err := lib.GetSymbol("host_statistics")
	if err != nil {
		return nil, fmt.Errorf("failed to get host_statistics: %v", err)
	}

	// 调用 mach_host_self
	hostPort, _, _ := purego.SyscallN(machHostSelfFunc)

	// 准备主机 CPU 负载信息
	var cpuLoadInfo hostCpuLoadInfoData
	count := uint32(HOST_CPU_LOAD_INFO_COUNT)

	// 调用 host_statistics
	ret, _, _ := purego.SyscallN(hostStatisticsFunc,
		hostPort,
		uintptr(HOST_CPU_LOAD_INFO),
		uintptr(unsafe.Pointer(&cpuLoadInfo)),
		uintptr(unsafe.Pointer(&count)))

	if ret != KERN_SUCCESS {
		return nil, fmt.Errorf("host_statistics failed with error: %d", ret)
	}

	// 将 ticks 转换为时间（秒）
	cpuTimes := TimesStat{
		CPU:    "cpu-total",
		User:   float64(cpuLoadInfo.cpuTicks[CPU_STATE_USER]) / clocksPerSec,
		System: float64(cpuLoadInfo.cpuTicks[CPU_STATE_SYSTEM]) / clocksPerSec,
		Idle:   float64(cpuLoadInfo.cpuTicks[CPU_STATE_IDLE]) / clocksPerSec,
		Nice:   float64(cpuLoadInfo.cpuTicks[CPU_STATE_NICE]) / clocksPerSec,
	}

	return []TimesStat{cpuTimes}, nil
}

func perCPUTimes(lib *Library) ([]TimesStat, error) {
	// 获取 CPU 核心数
	numCPU := runtime.NumCPU()
	ret := make([]TimesStat, numCPU)

	// 对于每个 CPU 核心，我们使用相同的总体数据
	// 这是一个简化的实现，因为 macOS 的 per-CPU 数据获取比较复杂
	allTimes, err := allCPUTimes(lib)
	if err != nil {
		return nil, err
	}

	if len(allTimes) > 0 {
		totalTime := allTimes[0]
		for i := 0; i < numCPU; i++ {
			ret[i] = TimesStat{
				CPU:    fmt.Sprintf("cpu%d", i),
				User:   totalTime.User / float64(numCPU),
				System: totalTime.System / float64(numCPU),
				Idle:   totalTime.Idle / float64(numCPU),
				Nice:   totalTime.Nice / float64(numCPU),
			}
		}
	}

	return ret, nil
}

// PercentWithContext 计算指定进程在给定时间间隔内的CPU使用率
func (p *Process) PercentWithContext(ctx context.Context, interval time.Duration) (float64, error) {
	if interval <= 0 {
		return p.percentUsedFromLastCall(ctx)
	}

	// 获取第一次测量
	firstTimes, err := p.TimesWithContext(ctx)
	if err != nil {
		return 0, err
	}
	firstTime := time.Now()

	// 等待指定时间间隔
	select {
	case <-time.After(interval):
	case <-ctx.Done():
		return 0, ctx.Err()
	}

	// 获取第二次测量
	secondTimes, err := p.TimesWithContext(ctx)
	if err != nil {
		return 0, err
	}
	secondTime := time.Now()

	// 计算时间差（秒）
	delta := secondTime.Sub(firstTime).Seconds()
	numCPU := runtime.NumCPU()

	return calculatePercent(firstTimes, secondTimes, delta, numCPU), nil
}

func (p *Process) percentUsedFromLastCall(ctx context.Context) (float64, error) {
	if p.lastCPUTimes == nil || p.lastCPUTime.IsZero() {
		// 第一次调用，只存储数据，返回 0
		times, err := p.TimesWithContext(ctx)
		if err != nil {
			return 0, err
		}
		p.lastCPUTimes = times
		p.lastCPUTime = time.Now()
		return 0, nil
	}

	// 获取当前数据
	currentTimes, err := p.TimesWithContext(ctx)
	if err != nil {
		return 0, err
	}
	currentTime := time.Now()

	// 计算使用率
	delta := currentTime.Sub(p.lastCPUTime).Seconds()
	numCPU := runtime.NumCPU()
	percent := calculatePercent(p.lastCPUTimes, currentTimes, delta, numCPU)

	// 更新存储的数据
	p.lastCPUTimes = currentTimes
	p.lastCPUTime = currentTime

	return percent, nil
}

func calculatePercent(t1, t2 *TimesStat, delta float64, numcpu int) float64 {
	if delta == 0 {
		return 0
	}

	delta_proc := t2.Total() - t1.Total()
	overall_percent := ((delta_proc / delta) / float64(numcpu)) * 100
	return overall_percent
}

// TimesWithContext 获取指定进程的 CPU 时间信息
func (p *Process) TimesWithContext(ctx context.Context) (*TimesStat, error) {
	var libPath string
	if runtime.GOOS == "darwin" {
		libPath = "/usr/lib/libSystem.B.dylib"
	} else {
		// FreeBSD
		libPath = "/lib/libc.so.7"
	}

	lib, err := loadLibrary(libPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load library: %v", err)
	}
	defer lib.Close()

	// 获取进程任务端口
	taskForPidFunc, err := lib.GetSymbol("task_for_pid")
	if err != nil {
		return nil, fmt.Errorf("failed to get task_for_pid: %v", err)
	}

	machTaskSelfFunc, err := lib.GetSymbol("mach_task_self")
	if err != nil {
		return nil, fmt.Errorf("failed to get mach_task_self: %v", err)
	}

	taskInfoFunc, err := lib.GetSymbol("task_info")
	if err != nil {
		return nil, fmt.Errorf("failed to get task_info: %v", err)
	}

	// 获取当前任务端口
	currentTask, _, _ := purego.SyscallN(machTaskSelfFunc)

	var targetTask uintptr
	if p.Pid == 0 {
		// 当前进程
		targetTask = currentTask
	} else {
		// 其他进程
		ret, _, _ := purego.SyscallN(taskForPidFunc,
			currentTask,
			uintptr(p.Pid),
			uintptr(unsafe.Pointer(&targetTask)))
		if ret != KERN_SUCCESS {
			return nil, fmt.Errorf("task_for_pid failed with error: %d", ret)
		}
	}

	// 获取进程任务信息
	var taskInfo procTaskInfo
	count := uint32(unsafe.Sizeof(taskInfo) / unsafe.Sizeof(uint32(0)))

	ret, _, _ := purego.SyscallN(taskInfoFunc,
		targetTask,
		uintptr(PROC_PIDTASKINFO),
		uintptr(unsafe.Pointer(&taskInfo)),
		uintptr(unsafe.Pointer(&count)))

	if ret != KERN_SUCCESS {
		return nil, fmt.Errorf("task_info failed with error: %d", ret)
	}

	// 转换为 TimesStat 结构
	var times TimesStat
	if runtime.GOOS == "darwin" {
		// macOS 使用 Mach absolute time units
		// 需要转换为秒
		type timebaseInfo struct {
			Numer uint32
			Denom uint32
		}

		machTimebaseInfoFunc, err := lib.GetSymbol("mach_timebase_info")
		if err != nil {
			return nil, fmt.Errorf("failed to get mach_timebase_info: %v", err)
		}

		var timebase timebaseInfo
		purego.SyscallN(machTimebaseInfoFunc, uintptr(unsafe.Pointer(&timebase)))

		// 转换 Mach time units 到纳秒，然后到秒
		nanoToSecond := 1e9
		times = TimesStat{
			CPU:    fmt.Sprintf("pid-%d", p.Pid),
			User:   float64(taskInfo.Total_user*uint64(timebase.Numer)) / float64(timebase.Denom) / float64(nanoToSecond),
			System: float64(taskInfo.Total_system*uint64(timebase.Numer)) / float64(timebase.Denom) / float64(nanoToSecond),
		}
	} else {
		// FreeBSD 使用微秒
		times = TimesStat{
			CPU:    fmt.Sprintf("pid-%d", p.Pid),
			User:   float64(taskInfo.Total_user) / 1e6, // 微秒转秒
			System: float64(taskInfo.Total_system) / 1e6,
		}
	}

	return &times, nil
}

// GetCPUInfo 获取系统 CPU 信息
func GetCPUInfo() (CPUInfo, error) {
	times, err := TimesWithContext(context.Background(), false)
	if err != nil {
		return CPUInfo{}, err
	}

	if len(times) == 0 {
		return CPUInfo{}, errors.New("no CPU times available")
	}

	t := times[0]
	return CPUInfo{
		User:   t.User,
		System: t.System,
		Idle:   t.Idle,
		IOWait: t.Iowait,
		Total:  t.Total(),
	}, nil
}

// GetProcessCPUInfo 获取指定进程的 CPU 信息
func GetProcessCPUInfo(pid int) (ProcessCPUInfo, error) {
	p := &Process{Pid: int32(pid)}
	times, err := p.TimesWithContext(context.Background())
	if err != nil {
		return ProcessCPUInfo{}, err
	}

	return ProcessCPUInfo{
		User:   times.User,
		System: times.System,
		Total:  times.Total(),
	}, nil
}
