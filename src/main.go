package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	version     = "v1.7.3"
	copyright   = "Copyright (c) 2025. All rights reserved."
	programName = "TCPing"
)

type Statistics struct {
	sync.Mutex
	sentCount      int64
	respondedCount int64
	minTime        float64
	maxTime        float64
	totalTime      float64
}

func (s *Statistics) update(elapsed float64, success bool) {
	s.Lock()
	defer s.Unlock()

	s.sentCount++

	if !success {
		return
	}

	s.respondedCount++
	s.totalTime += elapsed

	// 首次响应特殊处理
	if s.respondedCount == 1 {
		s.minTime = elapsed
		s.maxTime = elapsed
		return
	}

	// 更新最小和最大时间
	if elapsed < s.minTime {
		s.minTime = elapsed
	}
	if elapsed > s.maxTime {
		s.maxTime = elapsed
	}
}

func (s *Statistics) getStats() (sent, responded int64, min, max, avg float64) {
	s.Lock()
	defer s.Unlock()

	avg = 0.0
	if s.respondedCount > 0 {
		avg = s.totalTime / float64(s.respondedCount)
	}

	return s.sentCount, s.respondedCount, s.minTime, s.maxTime, avg
}

type Options struct {
	UseIPv4     bool
	UseIPv6     bool
	Count       int
	Interval    int // 请求间隔（毫秒）
	Timeout     int
	ColorOutput bool
	VerboseMode bool
	ShowVersion bool
	ShowHelp    bool
	Port        int
}

func handleError(err error, exitCode int) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误: %v\n", err)
		os.Exit(exitCode)
	}
}

func printHelp() {
	fmt.Printf(`%s %s - TCP 连接测试工具

描述:
    %s 测试到目标主机和端口的TCP连接性。

用法: 
    tcping [选项] <主机> [端口]      (默认端口: 80)

选项:
    -4, --ipv4              强制使用 IPv4
    -6, --ipv6              强制使用 IPv6
    -n, --count <次数>      发送请求的次数 (默认: 无限)
    -p, --port <端口>       指定要连接的端口 (默认: 80)
    -t, --interval <毫秒>    连接超时 (默认: 1000毫秒)
    -w, --timeout <毫秒>    连接超时 (默认: 1000毫秒)
    -c, --color             启用彩色输出
    -v, --verbose           启用详细模式，显示更多连接信息
    -V, --version           显示版本信息
    -h, --help              显示此帮助信息

示例:
    tcping google.com                	# 基本用法 (默认端口 80)
    tcping google.com 80             	# 基本用法指定端口
    tcping -p 443 google.com         	# 使用-p参数指定端口
    tcping -4 -n 5 8.8.8.8 443       	# IPv4, 5次请求
    tcping -w 2000 example.com 22    	# 2秒超时
    tcping -c -v example.com 443     	# 彩色输出和详细模式

`, programName, version, programName)
}

func printVersion() {
	fmt.Printf("%s 版本 %s\n", programName, version)
	fmt.Println(copyright)
}

func resolveAddress(address string, useIPv4, useIPv6 bool) (string, error) {
	// 尝试标准IP解析
	if ip := net.ParseIP(address); ip != nil {
		isV4 := ip.To4() != nil
		if useIPv4 && !isV4 {
			return "", fmt.Errorf("地址 %s 不是 IPv4 地址", address)
		}
		if useIPv6 && isV4 {
			return "", fmt.Errorf("地址 %s 不是 IPv6 地址", address)
		}
		if !isV4 {
			return "[" + ip.String() + "]", nil
		}
		return ip.String(), nil
	}

	// 最后尝试DNS解析
	ipList, err := net.LookupIP(address)
	if err != nil {
		return "", fmt.Errorf("解析 %s 失败: %v", address, err)
	}

	if len(ipList) == 0 {
		return "", fmt.Errorf("未找到 %s 的 IP 地址", address)
	}

	if useIPv4 {
		for _, ip := range ipList {
			if ip.To4() != nil {
				return ip.String(), nil
			}
		}
		return "", fmt.Errorf("未找到 %s 的 IPv4 地址", address)
	}

	if useIPv6 {
		for _, ip := range ipList {
			if ip.To4() == nil {
				return "[" + ip.String() + "]", nil
			}
		}
		return "", fmt.Errorf("未找到 %s 的 IPv6 地址", address)
	}

	// 如果没有强制指定IP版本，优先使用IPv4地址
	// 首先查找IPv4地址
	for _, ip := range ipList {
		if ip.To4() != nil {
			return ip.String(), nil
		}
	}

	// 如果没有找到IPv4地址，使用第一个IPv6地址
	for _, ip := range ipList {
		if ip.To4() == nil {
			return "[" + ip.String() + "]", nil
		}
	}

	// 理论上不应该到达这里，因为ipList不为空
	ip := ipList[0]
	if ip.To4() == nil {
		return "[" + ip.String() + "]", nil
	}
	return ip.String(), nil
}

func getIPType(address string) (isIPv4, isIPv6 bool) {
	ip := net.ParseIP(address)
	if ip == nil {
		return false, false
	}
	return ip.To4() != nil, ip.To4() == nil
}

func pingOnce(ctx context.Context, address, port string, timeout int, stats *Statistics, seq int, ip string,
	opts *Options) {
	// 创建可取消的连接上下文，继承父上下文
	dialCtx, dialCancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Millisecond)
	defer dialCancel()

	start := time.Now()
	var d net.Dialer
	conn, err := d.DialContext(dialCtx, "tcp", address+":"+port)
	elapsed := float64(time.Since(start).Microseconds()) / 1000.0

	// 检查是否因为主上下文取消而失败
	if ctx.Err() == context.Canceled {
		msg := "\n操作被中断, 连接尝试已中止\n"
		fmt.Print(infoText(msg, opts.ColorOutput))
		return
	}

	success := err == nil
	stats.update(elapsed, success)

	if !success {
		msg := fmt.Sprintf("TCP连接失败 %s:%s: seq=%d 错误=%v\n", ip, port, seq, err)
		fmt.Print(errorText(msg, opts.ColorOutput))

		if opts.VerboseMode {
			fmt.Printf("  详细信息: 连接尝试耗时 %.2fms, 目标 %s:%s\n", elapsed, address, port)
		}
		return
	}

	// 确保连接被关闭
	defer conn.Close()
	msg := fmt.Sprintf("从 %s:%s 收到响应: seq=%d time=%.2fms\n", ip, port, seq, elapsed)
	fmt.Print(successText(msg, opts.ColorOutput))

	if opts.VerboseMode {
		localAddr := conn.LocalAddr().String()
		fmt.Printf("  详细信息: 本地地址=%s, 远程地址=%s:%s\n", localAddr, ip, port)
	}
}

func printTCPingStatistics(stats *Statistics) {
	sent, responded, min, max, avg := stats.getStats()

	fmt.Printf("\n\n--- 目标主机 TCP ping 统计 ---\n")

	if sent > 0 {
		lossRate := float64(sent-responded) / float64(sent) * 100
		fmt.Printf("已发送 = %d, 已接收 = %d, 丢失 = %d (%.1f%% 丢失)\n",
			sent, responded, sent-responded, lossRate)

		if responded > 0 {
			fmt.Printf("往返时间(RTT): 最小 = %.2fms, 最大 = %.2fms, 平均 = %.2fms\n",
				min, max, avg)
		}
	}
}

func colorText(text, colorCode string, useColor bool) string {
	if !useColor {
		return text
	}
	return "\033[" + colorCode + "m" + text + "\033[0m"
}

func successText(text string, useColor bool) string {
	return colorText(text, "32", useColor) // 绿色
}

func errorText(text string, useColor bool) string {
	return colorText(text, "31", useColor) // 红色
}

func infoText(text string, useColor bool) string {
	return colorText(text, "36", useColor) // 青色
}

func setupFlags(opts *Options) {
	// 定义命令行标志，同时设置短选项和长选项
	flag.BoolVar(&opts.UseIPv4, "4", false, "使用 IPv4 地址")
	flag.BoolVar(&opts.UseIPv4, "ipv4", false, "使用 IPv4 地址")
	flag.BoolVar(&opts.UseIPv6, "6", false, "使用 IPv6 地址")
	flag.BoolVar(&opts.UseIPv6, "ipv6", false, "使用 IPv6 地址")
	flag.IntVar(&opts.Count, "n", 0, "发送请求次数 (默认: 无限)")
	flag.IntVar(&opts.Count, "count", 0, "发送请求次数 (默认: 无限)")
	flag.IntVar(&opts.Interval, "t", 1000, "请求间隔（毫秒）")
	flag.IntVar(&opts.Interval, "interval", 1000, "请求间隔（毫秒）")
	flag.IntVar(&opts.Timeout, "w", 1000, "连接超时（毫秒）")
	flag.IntVar(&opts.Timeout, "timeout", 1000, "连接超时（毫秒）")
	flag.IntVar(&opts.Port, "p", 0, "指定要连接的端口 (默认: 80)")
	flag.IntVar(&opts.Port, "port", 0, "指定要连接的端口 (默认: 80)")
	flag.BoolVar(&opts.ColorOutput, "c", false, "启用彩色输出")
	flag.BoolVar(&opts.ColorOutput, "color", false, "启用彩色输出")
	flag.BoolVar(&opts.VerboseMode, "v", false, "启用详细模式")
	flag.BoolVar(&opts.VerboseMode, "verbose", false, "启用详细模式")
	flag.BoolVar(&opts.ShowVersion, "V", false, "显示版本信息")
	flag.BoolVar(&opts.ShowVersion, "version", false, "显示版本信息")
	flag.BoolVar(&opts.ShowHelp, "h", false, "显示帮助信息")
	flag.BoolVar(&opts.ShowHelp, "help", false, "显示帮助信息")

	flag.Parse()
}

// 新增集中的参数验证函数
func validateOptions(opts *Options, args []string) (string, string, error) {
	// 验证基本选项
	if opts.UseIPv4 && opts.UseIPv6 {
		return "", "", errors.New("无法同时使用 -4 和 -6 标志")
	}

	if opts.Interval < 0 {
		return "", "", errors.New("间隔时间不能为负值")
	}

	if opts.Timeout < 0 {
		return "", "", errors.New("超时时间不能为负值")
	}

	// 验证主机参数
	if len(args) < 1 {
		return "", "", errors.New("需要提供主机参数\n\n用法: tcping [选项] <主机> [端口]\n尝试 'tcping -h' 获取更多信息")
	}

	host := args[0]
	port := "80" // 默认端口为 80

	// 优先级：命令行直接指定的端口 > -p参数指定的端口 > 默认端口80
	if len(args) > 1 {
		port = args[1]
	} else if opts.Port > 0 {
		// 如果通过-p参数指定了端口且命令行没有直接指定端口，则使用-p参数的值
		port = strconv.Itoa(opts.Port)
	}

	// 验证端口
	if portNum, err := strconv.Atoi(port); err != nil {
		return "", "", fmt.Errorf("端口号格式无效")
	} else if portNum <= 0 || portNum > 65535 {
		return "", "", fmt.Errorf("端口号必须在 1 到 65535 之间")
	}

	return host, port, nil
}

func main() {
	// 创建选项结构
	opts := &Options{}

	// 设置和解析命令行参数
	setupFlags(opts)

	// 处理帮助和版本信息选项，这些选项优先级最高
	if opts.ShowHelp {
		printHelp()
		os.Exit(0)
	}

	if opts.ShowVersion {
		printVersion()
		os.Exit(0)
	}

	// 集中验证所有参数
	host, port, err := validateOptions(opts, flag.Args())
	if err != nil {
		handleError(err, 1)
	}

	// 确定使用IPv4还是IPv6
	hostIsIPv4, hostIsIPv6 := getIPType(host)
	useIPv4 := opts.UseIPv4 || (!opts.UseIPv6 && hostIsIPv4)
	useIPv6 := opts.UseIPv6 || hostIsIPv6

	// 保存原始主机名用于显示
	originalHost := host

	// 解析IP地址
	address, err := resolveAddress(host, useIPv4, useIPv6)
	if err != nil {
		handleError(err, 1)
	}

	// 提取IP地址用于显示
	ipType := "IPv4"
	ipAddress := address
	if strings.HasPrefix(address, "[") && strings.HasSuffix(address, "]") {
		ipType = "IPv6"
		ipAddress = address[1 : len(address)-1]
	}

	fmt.Printf("正在对 %s (%s - %s) 端口 %s 执行 TCP Ping\n", originalHost, ipType, ipAddress, port)
	stats := &Statistics{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 创建信号捕获通道
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)

	// 使用 WaitGroup 来确保后台 goroutine 正确退出
	var wg sync.WaitGroup
	wg.Add(1)

	// 启动ping协程
	go func() {
		defer wg.Done()

		for i := 0; opts.Count == 0 || i < opts.Count; i++ {
			// 检查上下文是否已取消
			select {
			case <-ctx.Done():
				return
			default:
			}

			// 执行ping
			pingOnce(ctx, address, port, opts.Timeout, stats, i, ipAddress, opts)

			// 检查是否完成所有请求
			if opts.Count != 0 && i == opts.Count-1 {
				break
			}

			// 等待下一次ping的间隔
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(opts.Interval) * time.Millisecond):
			}
		}
	}()

	// 等待中断信号或完成
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-interrupt:
		fmt.Printf("\n操作被中断。\n")
		cancel()
	case <-done:
		// 正常完成
	}
	printTCPingStatistics(stats)
}
