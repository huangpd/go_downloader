package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/tidwall/gjson"
)

// DownloadTask 下载任务结构
type DownloadTask struct {
	URL      string `json:"url"`
	SavePath string `json:"save_path"`
}

// DownloadInfo 存储下载信息
type DownloadInfo struct {
	URL           string        `json:"url"`
	FilePath      string        `json:"file_path"`
	FileSize      int64         `json:"file_size"`
	ChunkSize     int64         `json:"chunk_size"`
	Chunks        []Chunk       `json:"chunks"`
	CreatedAt     time.Time     `json:"created_at"`
	LastResumedAt time.Time     `json:"last_resumed_at"`
	Attempts      int           `json:"attempts"`
	SingleThread  bool          `json:"single_thread"`
	Task          *DownloadTask `json:"task"` // 关联的任务信息
}

// Chunk 存储分片信息
type Chunk struct {
	Index         int       `json:"index"`
	Start         int64     `json:"start"`
	End           int64     `json:"end"`
	Downloaded    int64     `json:"downloaded"`
	Completed     bool      `json:"completed"`
	FailCount     int       `json:"fail_count"`
	LastError     string    `json:"last_error,omitempty"`
	LastAttemptAt time.Time `json:"last_attempt_at"`
}

// DownloadService 下载服务
type DownloadService struct {
	redisClient     *redis.Client
	ctx             context.Context
	cancel          context.CancelFunc
	maxConcurrent   int
	semaphore       chan struct{} // 并发控制信号量
	listenQueue     string
	failedTasksHash string
	group           string
}

// NewDownloadService 创建下载服务
func NewDownloadService(redisAddr, redisPassword, listenQueue, failedTasksHash, group string, maxConcurrent int) *DownloadService {
	ctx, cancel := context.WithCancel(context.Background())

	rdb := redis.NewClient(&redis.Options{
		Addr:     redisAddr,
		Password: redisPassword,
		DB:       0,
	})

	return &DownloadService{
		redisClient:     rdb,
		ctx:             ctx,
		cancel:          cancel,
		maxConcurrent:   maxConcurrent,
		semaphore:       make(chan struct{}, maxConcurrent),
		listenQueue:     listenQueue,
		failedTasksHash: failedTasksHash,
		group:           group,
	}
}

// Start 启动服务
func (ds *DownloadService) Start() {
	fmt.Println("🚀 Go下载服务启动中...")
	fmt.Printf("📡 Redis地址: %s\n", ds.redisClient.Options().Addr)
	fmt.Printf("🔄 最大并发下载数: %d\n", ds.maxConcurrent)

	// 测试Redis连接
	_, err := ds.redisClient.Ping(ds.ctx).Result()
	if err != nil {
		log.Fatalf("❌ Redis连接失败: %v", err)
	}
	fmt.Println("✅ Redis连接成功")

	// 开始监听Redis队列
	ds.listenRedisQueue()
}

// listenRedisQueue 监听Redis队列
func (ds *DownloadService) listenRedisQueue() {
	fmt.Printf("👂 开始监听Redis队列: %s\n", ds.listenQueue)

	for {
		select {
		case <-ds.ctx.Done():
			fmt.Println("📱 服务停止")
			return
		default:
			// 从Redis队列获取任务
			result, err := ds.redisClient.BLPop(ds.ctx, 5*time.Second, ds.listenQueue).Result()
			if err != nil {
				if err == redis.Nil {
					// 队列为空，继续等待
					continue
				}
				fmt.Printf("❌ Redis获取数据失败: %v\n", err)
				time.Sleep(5 * time.Second)
				continue
			}

			if len(result) < 2 {
				fmt.Println("⚠️  Redis数据格式错误")
				continue
			}

			// 解析任务数据
			taskData := result[1]
			task, err := ds.parseTask(taskData)
			if err != nil {
				fmt.Printf("❌ 解析任务失败: %v\n", err)
				continue
			}

			// 显示并发状态
			activeDownloads := ds.maxConcurrent - len(ds.semaphore)
			fmt.Printf("📥 收到下载任务: %s (当前活跃: %d/%d)\n", task.SavePath, activeDownloads, ds.maxConcurrent)

			// 使用信号量控制并发数
			go func(task *DownloadTask, taskData string) {
				ds.semaphore <- struct{}{}		// 获取信号量
				defer func() { <-ds.semaphore }() // 释放信号量
				ds.processDownloadTask(task, taskData)
			}(task, taskData)
		}
	}
}

// parseTask 解析任务数据
func (ds *DownloadService) parseTask(data string) (*DownloadTask, error) {
	task := &DownloadTask{}

	// 使用gjson解析JSON
	if !gjson.Valid(data) {
		return nil, fmt.Errorf("无效的JSON格式")
	}

	result := gjson.Parse(data)
	task.URL = result.Get("url").String()
	task.SavePath = result.Get("save_path").String()

	// 验证必要字段
	if task.URL == "" || task.SavePath == "" {
		return nil, fmt.Errorf("缺少必要字段: url=%s, save_path=%s", task.URL, task.SavePath)
	}

	return task, nil
}

// processDownloadTask 处理下载任务
func (ds *DownloadService) processDownloadTask(task *DownloadTask, taskData string) {
	startTime := time.Now()

	// 构建完整文件路径
	fullPath := task.SavePath

	fmt.Printf("🔄 开始下载: %s -> %s\n", task.URL, fullPath)

	// 创建下载器
	downloader := NewDownloader(task.URL, fullPath)
	downloader.SetTask(task) // 设置任务信息

	// 执行下载
	err := downloader.Download(task.URL, fullPath)

	// 如果因为无法确定文件大小而失败，则自动切换到简单模式
	if err != nil && strings.Contains(err.Error(), "无法确定文件大小") {
		fmt.Printf("\n⚠️  多线程模式失败 (无法确定文件大小)，自动切换到简单下载模式...\n")
		simpleDownloader := NewSimpleDownloader(task.URL, fullPath)
		err = simpleDownloader.Download() // 切换到简单下载器重试
	}

	downloadTime := time.Since(startTime)

	if err != nil {
		fmt.Printf("❌ 下载失败: %s, 耗时: %v, 错误: %v\n", task.SavePath, downloadTime, err)
		ds.recordFailureInRedis(task, taskData, err.Error()) // 记录失败任务到Hash
	} else {
		// 获取文件大小
		fileInfo, _ := os.Stat(fullPath)
		var fileSize int64
		if fileInfo != nil {
			fileSize = fileInfo.Size()
		}

		fmt.Printf("✅ 下载成功: %s, 大小: %.2f MB, 耗时: %v\n",
			task.SavePath, float64(fileSize)/1024/1024, downloadTime)
		ds.recordSuccessInRedis(task, fileSize, downloadTime)
	}
}

// SuccessInfo 存储成功任务的详细信息
type SuccessInfo struct {
	URL        string    `json:"url"`
	SavePath   string    `json:"save_path"`
	FileSize   int64     `json:"file_size"`
	Duration   float64   `json:"duration"`
	FinishedAt time.Time `json:"finished_at"`
}

// recordSuccessInRedis 将成功任务记录到Redis，并设置过期时间
func (ds *DownloadService) recordSuccessInRedis(task *DownloadTask, fileSize int64, duration time.Duration) {
	var key string
	if ds.group != "" {
		key = fmt.Sprintf("success:%s:%s", ds.group, task.URL)
	} else {
		key = fmt.Sprintf("success:%s", task.URL)
	}

	info := SuccessInfo{
		URL:        task.URL,
		SavePath:   task.SavePath,
		FileSize:   fileSize,
		Duration:   duration.Seconds(),
		FinishedAt: time.Now(),
	}

	valueBytes, err := json.Marshal(info)
	if err != nil {
		fmt.Printf("⚠️  序列化成功详情失败: %v\n", err)
		return
	}

	// 使用 SETEX 将成功记录存入Redis，并设置24小时过期
	err = ds.redisClient.SetEX(ds.ctx, key, string(valueBytes), 24*time.Hour).Err()
	if err != nil {
		fmt.Printf("⚠️  记录成功任务到Redis失败: %v\n", err)
	} else {
		fmt.Printf("📋 成功任务已记录到Redis (Key: %s, 24小时后过期)\n", key)
	}
}

// FailureInfo 存储失败任务的详细信息
type FailureInfo struct {
	TaskData     json.RawMessage `json:"task_data"`
	ErrorMessage string          `json:"error_message"`
	FailedAt     time.Time       `json:"failed_at"`
}

// recordFailureInRedis 将失败任务记录到Redis Hash
func (ds *DownloadService) recordFailureInRedis(task *DownloadTask, taskData string, errMsg string) {
	info := FailureInfo{
		TaskData:     json.RawMessage(taskData),
		ErrorMessage: errMsg,
		FailedAt:     time.Now(),
	}

	valueBytes, err := json.Marshal(info)
	if err != nil {
		fmt.Printf("⚠️  序列化失败详情失败: %v\n", err)
		return
	}

	// 使用 HSET 将失败任务存入Hash，以URL为field
	err = ds.redisClient.HSet(ds.ctx, ds.failedTasksHash, task.URL, string(valueBytes)).Err()
	if err != nil {
		fmt.Printf("⚠️  记录失败任务到Redis Hash失败: %v\n", err)
	} else {
		fmt.Printf("↪️  失败任务已记录到Hash: %s\n", ds.failedTasksHash)
	}
}

// Downloader 下载器结构体
type Downloader struct {
	info              *DownloadInfo
	infoFile          string
	client            *http.Client
	fallbackClient    *http.Client
	mu                sync.RWMutex
	startTime         time.Time
	lastBytes         int64
	lastTime          time.Time
	progressTicker    *time.Ticker
	ctx               context.Context
	cancel            context.CancelFunc
	maxRetries        int
	baseRetryDelay    time.Duration
	maxRetryDelay     time.Duration
	connectionTimeout time.Duration
	readTimeout       time.Duration
	maxConcurrent     int
	singleThreadMode  bool
	task              *DownloadTask // 关联的任务
}

// SetTask 设置任务信息
func (d *Downloader) SetTask(task *DownloadTask) {
	d.task = task
}

// NewDownloader 创建新的下载器
func NewDownloader(url, filePath string) *Downloader {
	infoFile := filePath + ".download"
	now := time.Now()
	ctx, cancel := context.WithCancel(context.Background())

	// 创建高度定制的HTTP客户端
	transport := &http.Transport{
		// 强制使用 HTTP/1.1
		ForceAttemptHTTP2: false,

		// 连接相关
		DialContext: (&net.Dialer{
			Timeout:   15 * time.Second, // TCP连接超时
			KeepAlive: 30 * time.Second, // TCP keepalive
		}).DialContext,
		// 最大空闲连接数
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20, // 视并发量和服务器能力可调大
		// 空闲连接最大存活时间
		IdleConnTimeout: 90 * time.Second,
		// TLS 握手超时
		TLSHandshakeTimeout: 10 * time.Second,
		// 100-Continue等待超时
		ExpectContinueTimeout: 1 * time.Second,
		// 响应头读取超时（非常重要，防止服务端卡顿）
		ResponseHeaderTimeout: 15 * time.Second,
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   0, // 移除总超时，让单独的读写超时控制
	}

	// 创建备用的单线程客户端（配置也要优化）
	fallbackTransport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          5,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		MaxIdleConnsPerHost:   2,
		DisableCompression:    false,
		ResponseHeaderTimeout: 30 * time.Second,
	}

	fallbackClient := &http.Client{
		Transport: fallbackTransport,
		Timeout:   0,
	}

	return &Downloader{
		infoFile:          infoFile,
		client:            client,
		fallbackClient:    fallbackClient,
		startTime:         now,
		lastTime:          now,
		lastBytes:         0,
		ctx:               ctx,
		cancel:            cancel,
		maxRetries:        3,                // 减少重试次数以提高速度
		baseRetryDelay:    1 * time.Second,  // 减少重试延迟
		maxRetryDelay:     30 * time.Second, // 减少最大延迟
		connectionTimeout: 30 * time.Second,
		readTimeout:       60 * time.Second,
		maxConcurrent:     8, // 增加默认并发数
		singleThreadMode:  false,
	}
}

// 检查服务器是否支持断点续传
func (d *Downloader) checkResumeSupport(url string) (int64, bool, error) {
	// 使用更长的超时进行探测
	ctx, cancel := context.WithTimeout(d.ctx, 60*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "HEAD", url, nil)
	if err != nil {
		return 0, false, err
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("Accept", "*/*")
	// 允许连接复用以提高性能

	resp, err := d.client.Do(req)
	if err != nil {
		fmt.Printf("⚠️  HEAD请求失败，尝试GET请求: %v\n", err)
		// 如果HEAD失败，尝试GET请求获取文件大小
		return d.getFileSizeWithGET(url)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, false, fmt.Errorf("服务器返回状态码: %d", resp.StatusCode)
	}

	contentLength := resp.Header.Get("Content-Length")
	if contentLength == "" {
		fmt.Println("⚠️  无法从HEAD请求获取文件大小，尝试GET请求")
		return d.getFileSizeWithGET(url)
	}

	size, err := strconv.ParseInt(contentLength, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("解析文件大小失败: %v", err)
	}

	// 检查是否支持Range请求
	acceptRanges := resp.Header.Get("Accept-Ranges")
	supportsRange := acceptRanges == "bytes"

	// 额外检查：尝试一个小范围请求
	if supportsRange {
		supportsRange = d.testRangeRequest(url)
	}

	if !supportsRange {
		fmt.Println("⚠️  服务器不支持断点续传，将使用单线程下载")
		d.singleThreadMode = true
		d.maxConcurrent = 1
	}

	return size, supportsRange, nil
}

// 通过GET请求获取文件大小
func (d *Downloader) getFileSizeWithGET(url string) (int64, bool, error) {
	ctx, cancel := context.WithTimeout(d.ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return 0, false, err
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("Range", "bytes=0-1023") // 只请求前1KB

	resp, err := d.client.Do(req)
	if err != nil {
		return 0, false, err
	}
	defer resp.Body.Close()

	var size int64
	if resp.StatusCode == http.StatusPartialContent {
		// 支持Range请求
		contentRange := resp.Header.Get("Content-Range")
		if contentRange != "" {
			// 解析 "bytes 0-1023/总大小" 格式
			parts := strings.Split(contentRange, "/")
			if len(parts) == 2 {
				if sizeStr := parts[1]; sizeStr != "*" {
					size, err = strconv.ParseInt(sizeStr, 10, 64)
					if err == nil {
						return size, true, nil
					}
				}
			}
		}
	}

	// 如果不支持Range或解析失败，尝试获取Content-Length
	contentLength := resp.Header.Get("Content-Length")
	if contentLength != "" {
		size, err = strconv.ParseInt(contentLength, 10, 64)
		if err == nil {
			return size, false, nil // 不支持Range
		}
	}

	return 0, false, fmt.Errorf("无法确定文件大小")
}

// 测试Range请求是否真正有效
func (d *Downloader) testRangeRequest(url string) bool {
	ctx, cancel := context.WithTimeout(d.ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return false
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("Range", "bytes=0-1023")

	resp, err := d.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	return resp.StatusCode == http.StatusPartialContent
}

// 智能分片策略
func (d *Downloader) calculateChunks(fileSize int64, supportsRange bool) (int64, int) {
	if !supportsRange {
		return fileSize, 1
	}

	var chunkSize int64
	var numChunks int

	switch {
	case fileSize < 5*1024*1024:
		chunkSize = fileSize
		numChunks = 1
	case fileSize < 50*1024*1024:
		chunkSize = 2 * 1024 * 1024
		numChunks = int((fileSize + chunkSize - 1) / chunkSize)
		d.maxConcurrent = 2
	case fileSize < 500*1024*1024:
		chunkSize = 8 * 1024 * 1024
		numChunks = int((fileSize + chunkSize - 1) / chunkSize)
		d.maxConcurrent = 3
	case fileSize < 2*1024*1024*1024:
		chunkSize = 16 * 1024 * 1024
		numChunks = int((fileSize + chunkSize - 1) / chunkSize)
		d.maxConcurrent = 4
	default:
		chunkSize = 32 * 1024 * 1024
		numChunks = int((fileSize + chunkSize - 1) / chunkSize)
		d.maxConcurrent = 5
	}

	if numChunks > 100 {
		numChunks = 100
		chunkSize = (fileSize + int64(numChunks) - 1) / int64(numChunks)
	}

	return chunkSize, numChunks
}

// 保存进度（带锁）
func (d *Downloader) saveProgress() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.info.LastResumedAt = time.Now()
	data, err := json.MarshalIndent(d.info, "", "  ")
	if err != nil {
		return err
	}

	// 原子写入
	tempFile := d.infoFile + ".tmp"
	err = os.WriteFile(tempFile, data, 0644)
	if err != nil {
		return err
	}

	return os.Rename(tempFile, d.infoFile)
}

// 加载进度
func (d *Downloader) loadProgress() error {
	data, err := os.ReadFile(d.infoFile)
	if err != nil {
		return err
	}

	err = json.Unmarshal(data, &d.info)
	if err != nil {
		return err
	}

	d.info.Attempts++
	fmt.Printf("📂 发现未完成的下载任务 (第%d次尝试)\n", d.info.Attempts)

	// 显示详细的恢复信息
	var totalDownloaded int64
	completedChunks := 0
	failedChunks := 0

	for _, chunk := range d.info.Chunks {
		totalDownloaded += chunk.Downloaded
		if chunk.Completed {
			completedChunks++
		} else if chunk.FailCount > 0 {
			failedChunks++
		}
	}

	progress := float64(totalDownloaded) / float64(d.info.FileSize) * 100
	fmt.Printf("📊 当前进度: %.1f%% (%.1f/%.1f MB)\n",
		progress,
		float64(totalDownloaded)/1024/1024,
		float64(d.info.FileSize)/1024/1024)
	fmt.Printf("📈 分片状态: %d已完成, %d需重试, %d总计\n",
		completedChunks, failedChunks, len(d.info.Chunks))

	if d.info.SingleThread {
		fmt.Println("⚡ 将使用单线程模式继续下载")
	}

	return nil
}

// 初始化下载任务
func (d *Downloader) initDownload(url, filePath string) error {
	fmt.Println("🔍 正在获取文件信息...")

	fileSize, supportsRange, err := d.checkResumeSupport(url)
	if err != nil {
		return fmt.Errorf("获取文件信息失败: %v", err)
	}

	chunkSize, numChunks := d.calculateChunks(fileSize, supportsRange)

	chunks := make([]Chunk, numChunks)
	for i := 0; i < numChunks; i++ {
		start := int64(i) * chunkSize
		end := start + chunkSize - 1
		if end >= fileSize {
			end = fileSize - 1
		}

		chunks[i] = Chunk{
			Index:         i,
			Start:         start,
			End:           end,
			Downloaded:    0,
			Completed:     false,
			FailCount:     0,
			LastAttemptAt: time.Time{},
		}
	}

	d.info = &DownloadInfo{
		URL:           url,
		FilePath:      filePath,
		FileSize:      fileSize,
		ChunkSize:     chunkSize,
		Chunks:        chunks,
		CreatedAt:     time.Now(),
		LastResumedAt: time.Now(),
		Attempts:      1,
		SingleThread:  d.singleThreadMode,
		Task:          d.task,
	}

	fmt.Printf("📄 文件大小: %.2f MB\n", float64(fileSize)/1024/1024)
	if numChunks == 1 {
		fmt.Println("🧩 下载模式: 单线程")
	} else {
		fmt.Printf("🧩 分片数量: %d (每片 %.1f MB)\n", numChunks, float64(chunkSize)/1024/1024)
		fmt.Printf("🚀 并发线程: %d\n", d.maxConcurrent)
	}

	return d.saveProgress()
}

// 智能重试延迟（更长的延迟）
func (d *Downloader) getRetryDelay(attempt int) time.Duration {
	if attempt <= 0 {
		return d.baseRetryDelay
	}

	// 更长的指数退避：2s, 4s, 8s, 16s, 32s, 64s, 120s...
	delay := d.baseRetryDelay * time.Duration(1<<uint(attempt-1))
	if delay > d.maxRetryDelay {
		delay = d.maxRetryDelay
	}

	// 添加更大的随机抖动
	jitter := time.Duration(float64(delay) * 0.3 * (0.5 + 0.5*float64(time.Now().UnixNano()%1000)/1000))
	return delay + jitter
}

// 下载单个分片（大幅优化版）
func (d *Downloader) downloadChunk(chunk *Chunk) error {
	if chunk.Completed {
		return nil
	}

	chunk.LastAttemptAt = time.Now()

	// 使用更长的超时时间
	ctx, cancel := context.WithTimeout(d.ctx, d.readTimeout)
	defer cancel()

	// 选择合适的客户端
	client := d.client
	if chunk.FailCount > 2 || d.singleThreadMode {
		client = d.fallbackClient // 失败多次后使用更保守的客户端
	}

	req, err := http.NewRequestWithContext(ctx, "GET", d.info.URL, nil)
	if err != nil {
		return fmt.Errorf("创建请求失败: %v", err)
	}

	// 设置请求头
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Encoding", "gzip, deflate, br")
	// 移除Connection: close，允许连接复用以提高性能

	// 设置Range头（只在多分片模式下）
	if len(d.info.Chunks) > 1 {
		rangeStart := chunk.Start + chunk.Downloaded
		rangeEnd := chunk.End
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", rangeStart, rangeEnd))
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	// 检查响应状态
	if len(d.info.Chunks) > 1 {
		if resp.StatusCode != http.StatusPartialContent {
			return fmt.Errorf("HTTP错误: %d %s (期望206)", resp.StatusCode, resp.Status)
		}
	} else {
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("HTTP错误: %d %s", resp.StatusCode, resp.Status)
		}
	}

	// 打开文件
	file, err := os.OpenFile(d.info.FilePath, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("打开文件失败: %v", err)
	}
	defer file.Close()

	// 定位到写入位置
	writePosition := chunk.Start + chunk.Downloaded
	_, err = file.Seek(writePosition, 0)
	if err != nil {
		return fmt.Errorf("文件定位失败: %v", err)
	}

	// 使用更大的缓冲区以提高性能
	bufferSize := 2 * 1024 * 1024 // 512KB缓冲区，提高下载速度
	if d.singleThreadMode {
		bufferSize = 4 * 1024 * 1024 // 单线程时使用1MB缓冲区
	}
	// 对于大文件使用更大的缓冲区
	if d.info.FileSize > 100*1024*1024 {
		bufferSize = 4 * 1024 * 1024 // 大文件使用1MB缓冲区
	}
	buffer := make([]byte, bufferSize)
	expectedBytes := (chunk.End - chunk.Start + 1) - chunk.Downloaded
	var totalRead int64

	// 更宽松的读取超时检查
	lastReadTime := time.Now()
	noProgressTimeout := 120 * time.Second // 120秒无进度才算超时

	for totalRead < expectedBytes {
		select {
		case <-ctx.Done():
			return fmt.Errorf("下载超时或取消")
		default:
		}

		// 检查无进度超时
		if time.Since(lastReadTime) > noProgressTimeout {
			return fmt.Errorf("长时间无数据传输")
		}

		// 设置读取截止时间
		if tcpConn, ok := resp.Body.(interface{ SetReadDeadline(time.Time) error }); ok {
			tcpConn.SetReadDeadline(time.Now().Add(30 * time.Second))
		}

		n, err := resp.Body.Read(buffer)
		if n > 0 {
			lastReadTime = time.Now()

			// 限制写入长度
			if totalRead+int64(n) > expectedBytes {
				n = int(expectedBytes - totalRead)
			}

			// 写入文件
			written, writeErr := file.Write(buffer[:n])
			if writeErr != nil {
				return fmt.Errorf("写入文件失败: %v", writeErr)
			}

			if written != n {
				return fmt.Errorf("写入不完整: 期望%d, 实际%d", n, written)
			}

			// 减少磁盘同步频率以提高性能
			if totalRead%(10*1024*1024) == 0 { // 每10MB刷新一次
				file.Sync()
			}

			// 更新进度
			d.mu.Lock()
			chunk.Downloaded += int64(n)
			d.mu.Unlock()

			totalRead += int64(n)
		}

		if err == io.EOF {
			break
		}

		if err != nil {
			// 网络错误时等待一下再返回
			time.Sleep(100 * time.Millisecond)
			return fmt.Errorf("读取数据失败: %v", err)
		}
	}

	// 验证下载完整性
	actualSize := chunk.Downloaded
	expectedSize := chunk.End - chunk.Start + 1

	if actualSize >= expectedSize {
		d.mu.Lock()
		chunk.Completed = true
		chunk.LastError = ""
		d.mu.Unlock()
		return nil
	}

	return fmt.Errorf("下载不完整: %d/%d bytes", actualSize, expectedSize)
}

// 进度显示
func (d *Downloader) printProgress() {
	d.progressTicker = time.NewTicker(time.Second)
	defer d.progressTicker.Stop()

	for {
		select {
		case <-d.progressTicker.C:
			d.mu.RLock()
			var totalDownloaded int64
			var completedChunks int
			var failedChunks int

			for _, chunk := range d.info.Chunks {
				totalDownloaded += chunk.Downloaded
				if chunk.Completed {
					completedChunks++
				} else if chunk.FailCount >= d.maxRetries {
					failedChunks++
				}
			}

			now := time.Now()
			progress := float64(totalDownloaded) / float64(d.info.FileSize) * 100

			// 计算速度
			timeDiff := now.Sub(d.lastTime).Seconds()
			bytesDiff := totalDownloaded - d.lastBytes
			currentSpeed := float64(bytesDiff) / timeDiff / 1024 / 1024

			// 计算ETA
			var eta string
			if currentSpeed > 0.01 {
				remainingBytes := d.info.FileSize - totalDownloaded
				etaSeconds := float64(remainingBytes) / (currentSpeed * 1024 * 1024)
				eta = formatDuration(time.Duration(etaSeconds * float64(time.Second)))
			} else {
				eta = "计算中..."
			}

			// 状态显示
			status := "下载中"
			if failedChunks > 0 {
				status = fmt.Sprintf("下载中(%d失败)", failedChunks)
			}

			fmt.Printf("\r🚀 %s %.1f%% | %.1f/%.1fMB | %.2fMB/s | ETA: %s | 分片: %d/%d     ",
				status,
				progress,
				float64(totalDownloaded)/1024/1024,
				float64(d.info.FileSize)/1024/1024,
				currentSpeed,
				eta,
				completedChunks,
				len(d.info.Chunks))

			d.lastBytes = totalDownloaded
			d.lastTime = now

			allCompleted := completedChunks == len(d.info.Chunks)
			d.mu.RUnlock()

			if allCompleted {
				fmt.Println("\n✅ 下载完成!")
				return
			}

		case <-d.ctx.Done():
			fmt.Println("\n❌ 下载已取消")
			return
		}
	}
}

// 格式化时间
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.0fs", d.Seconds())
	} else if d < time.Hour {
		return fmt.Sprintf("%.0fm%.0fs", d.Minutes(), math.Mod(d.Seconds(), 60))
	} else {
		hours := int(d.Hours())
		minutes := int(math.Mod(d.Minutes(), 60))
		return fmt.Sprintf("%dh%dm", hours, minutes)
	}
}

// 主下载逻辑
func (d *Downloader) Download(url, filePath string) error {
	// 尝试加载之前的进度
	err := d.loadProgress()
	if err != nil {
		fmt.Println("🆕 开始新的下载任务...")
		err = d.initDownload(url, filePath)
		if err != nil {
			return err
		}
	} else {
		// 恢复单线程模式设置
		if d.info.SingleThread {
			d.singleThreadMode = true
			d.maxConcurrent = 1
		}
	}

	// 创建目录
	dir := filepath.Dir(d.info.FilePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("创建目录失败: %v", err)
	}

	// 启动进度显示
	go d.printProgress()

	// 开始下载
	return d.performDownload()
}

// 执行下载（支持自动降级）
func (d *Downloader) performDownload() error {
	maxAttempts := 3

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		fmt.Printf("\n🔄 第%d次下载尝试...\n", attempt)

		success := d.downloadWithCurrentSettings()

		if success {
			// 删除进度文件
			os.Remove(d.infoFile)
			fmt.Println("🎉 所有分片下载完成!")
			return nil
		}

		// 检查失败情况并决定是否降级
		if attempt < maxAttempts {
			failureRate := d.getFailureRate()
			fmt.Printf("⚠️  当前失败率: %.1f%%\n", failureRate*100)

			if failureRate > 0.5 && !d.singleThreadMode {
				fmt.Println("🔄 失败率过高，切换到单线程模式...")
			d.switchToSingleThread()
			} else if !d.singleThreadMode && d.maxConcurrent > 1 {
				fmt.Printf("🔄 减少并发数: %d -> %d\n", d.maxConcurrent, 1)
			d.maxConcurrent = 1
			}

			// 等待一段时间再重试
			waitTime := time.Duration(attempt*30) * time.Second
			fmt.Printf("⏳ 等待%v后重试...\n", waitTime)
			time.Sleep(waitTime)
		}
	}

	failedChunks := d.getFailedChunksCount()
	fmt.Printf("💔 下载最终失败，%d个分片未完成。请检查网络连接后重新运行程序。\n", failedChunks)
	return fmt.Errorf("下载失败")
}

// 使用当前设置进行下载
func (d *Downloader) downloadWithCurrentSettings() bool {
	var wg sync.WaitGroup
	semaphore := make(chan struct{}, d.maxConcurrent)

	for i := range d.info.Chunks {
		chunk := &d.info.Chunks[i]

		if chunk.Completed {
			continue
		}

		wg.Add(1)
		go func(chunk *Chunk) {
			defer wg.Done()

			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			// 重试逻辑
			for attempt := 1; attempt <= d.maxRetries; attempt++ {
				select {
				case <-d.ctx.Done():
					return
				default:
				}

				err := d.downloadChunk(chunk)
				if err == nil {
					// 下载成功
					chunk.FailCount = 0
					chunk.LastError = ""
					break
				}

				// 记录错误
			d.mu.Lock()
			chunk.FailCount = attempt
			chunk.LastError = err.Error()
			d.mu.Unlock()

				if attempt < d.maxRetries {
					delay := d.getRetryDelay(attempt)

					// 只在调试模式下显示详细错误
					if strings.Contains(err.Error(), "context deadline exceeded") {
						fmt.Printf("⚠️  分片%d网络超时(第%d次)，%v后重试\n",
							chunk.Index, attempt, delay.Truncate(time.Second))
					} else {
						fmt.Printf("⚠️  分片%d失败(第%d次)，%v后重试: %v\n",
							chunk.Index, attempt, delay.Truncate(time.Second), err)
					}

					select {
					case <-time.After(delay):
					case <-d.ctx.Done():
						return
					}
				} else {
					fmt.Printf("❌ 分片%d最终失败: %v\n", chunk.Index, err)
				}
			}

			// 定期保存进度
			if chunk.Index%5 == 0 {
				d.saveProgress()
			}
		}(chunk)
	}

	// 等待所有任务完成
	wg.Wait()

	// 最终检查
	d.mu.Lock()
	allCompleted := true

	for _, chunk := range d.info.Chunks {
		if !chunk.Completed {
			allCompleted = false
			break
		}
	}
	d.mu.Unlock()

	// 保存最终状态
	d.saveProgress()

	return allCompleted
}

// 切换到单线程模式
func (d *Downloader) switchToSingleThread() {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.singleThreadMode = true
	d.maxConcurrent = 1
	d.info.SingleThread = true

	// 重新创建为单个大分片
	if len(d.info.Chunks) > 1 {
		// 计算总的已下载字节数
		var totalDownloaded int64
		for _, chunk := range d.info.Chunks {
			totalDownloaded += chunk.Downloaded
		}

		// 创建单个分片
		d.info.Chunks = []Chunk{
			{
				Index:         0,
				Start:         0,
				End:           d.info.FileSize - 1,
				Downloaded:    totalDownloaded,
				Completed:     false,
				FailCount:     0,
				LastError:     "",
				LastAttemptAt: time.Time{},
			},
		}

		d.info.ChunkSize = d.info.FileSize
	}
}

// 计算失败率
func (d *Downloader) getFailureRate() float64 {
	d.mu.RLock()
	defer d.mu.RUnlock()

	totalChunks := len(d.info.Chunks)
	failedChunks := 0

	for _, chunk := range d.info.Chunks {
		if !chunk.Completed && chunk.FailCount >= d.maxRetries {
			failedChunks++
		}
	}

	if totalChunks == 0 {
		return 0
	}

	return float64(failedChunks) / float64(totalChunks)
}

// 获取失败分片数量
func (d *Downloader) getFailedChunksCount() int {
	d.mu.RLock()
	defer d.mu.RUnlock()

	failedChunks := 0
	for _, chunk := range d.info.Chunks {
		if !chunk.Completed {
			failedChunks++
		}
	}

	return failedChunks
}

func main() {
	// 命令行模式的简单检测
	// 如果参数数量为3（程序名 + 2个参数），且第一个参数不以'-'开头，则认为是命令行模式
	if len(os.Args) == 3 && !strings.HasPrefix(os.Args[1], "-") {
		runCommandMode(os.Args[1], os.Args[2])
		return
	}

	// --- 服务模式 ---
	// 定义服务模式的flag
	redisAddr := flag.String("redis-addr", "localhost:6379", "Redis服务器地址")
	redisPassword := flag.String("redis-password", "", "Redis密码")
	maxConcurrent := flag.Int("max-concurrent-downloads", 5, "最大并发下载数")
	group := flag.String("group", "", "任务分组名，会作为Redis队列的前缀")

	// 设置自定义的Usage函数
	flag.Usage = showUsage
	// 解析参数
	flag.Parse()

	// 检查是否有非flag的额外参数（在服务模式下）
	if flag.NArg() > 0 {
		fmt.Printf("❌ 错误: 服务模式不支持位置参数 '%s'\n", flag.Arg(0))
		showUsage()
		os.Exit(1)
	}

	runServiceMode(*redisAddr, *redisPassword, *group, *maxConcurrent)
}

// runCommandMode 命令行模式
func runCommandMode(url, filePath string) {
	if url == "" || filePath == "" {
		fmt.Println("❌ URL和文件路径不能为空")
		os.Exit(1)
	}

	fmt.Println("🚀 Go多线程下载器 v3.1 - 命令行模式")
	fmt.Printf("📥 下载链接: %s\n", url)
	fmt.Printf("💾 保存路径: %s\n", filePath)
	fmt.Println()

	downloader := NewDownloader(url, filePath)
	startTime := time.Now()
	err := downloader.Download(url, filePath)

	// 如果因为无法确定文件大小而失败，则自动切换到简单模式
	if err != nil && strings.Contains(err.Error(), "无法确定文件大小") {
		fmt.Println("\n⚠️  无法确定文件大小，自动切换到简单下载模式...")
		simpleDownloader := NewSimpleDownloader(url, filePath)
		err = simpleDownloader.Download() // 切换到简单下载器重试
	}

	if err != nil {
		fmt.Printf("❌ 下载最终失败: %v\n", err)
		os.Exit(1)
	}

	downloadTime := time.Since(startTime)
	fmt.Printf("🎉 下载完成, 总耗时: %v\n", downloadTime.Truncate(time.Second))
}

// runServiceMode 服务模式
func runServiceMode(redisAddr, redisPassword, group string, maxConcurrent int) {
	fmt.Println("🚀 Go下载服务 v3.1 - Redis队列模式")

	// 根据group构造队列名和Hash名
	listenQueue := "go_download_urls"
	failedTasksHash := "go_download_failed_tasks"
	if group != "" {
		listenQueue = fmt.Sprintf("%s:%s", group, listenQueue)
		failedTasksHash = fmt.Sprintf("%s:%s", group, failedTasksHash)
		fmt.Printf("🏢 任务分组: %s\n", group)
	}

	// 创建并启动下载服务
	service := NewDownloadService(redisAddr, redisPassword, listenQueue, failedTasksHash, group, maxConcurrent)
	service.Start()
}

// showUsage 显示使用说明
func showUsage() {
	fmt.Println("Go多线程下载器 v3.1")
	fmt.Println("\n使用方式:")
	fmt.Println("  go_downloader <command> [arguments]")
	fmt.Println("\n支持的命令:")
	fmt.Println("  1. 服务模式 (默认): 启动一个长期运行的服务，监听Redis队列。")
	fmt.Println("     go_downloader [flags]")
	fmt.Println("  2. 命令行模式: 下载单个文件并立即退出。")
	fmt.Println("     go_downloader <URL> <SavePath>")

	fmt.Println("\n服务模式参数 (Flags):")
	fmt.Println("  --redis-addr string")
	fmt.Println("      Redis服务器地址 (默认: \"localhost:6379\")")
	fmt.Println("  --redis-password string")
	fmt.Println("      Redis密码 (默认: \"\")")
	fmt.Println("  --max-concurrent-downloads int")
	fmt.Println("      最大并发下载数 (默认: 5)")
	fmt.Println("  --group string")
	fmt.Println("      任务分组名，会作为Redis Key的前缀 (例如 'my-group')")

	fmt.Println("\n命令行模式示例:")
	fmt.Println("  go run main.go https://example.com/file.zip ./downloads/file.zip")
	fmt.Println("\n服务模式示例:")
	fmt.Println("  go run main.go --redis-addr=my-redis:6379 --group=my-group")
}

// SimpleDownloader 简化的单线程下载器 - 类似curl
type SimpleDownloader struct {
	client *http.Client
	url    string
	path   string
}

// NewSimpleDownloader 创建简化下载器
func NewSimpleDownloader(url, path string) *SimpleDownloader {
	// 简单高效的HTTP客户端配置
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:   false, // 使用HTTP/1.1，更稳定
		MaxIdleConns:        10,
		IdleConnTimeout:     30 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		MaxIdleConnsPerHost: 2,
		DisableCompression:  false,
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   0, // 无总超时
	}

	return &SimpleDownloader{
		client: client,
		url:    url,
		path:   path,
	}
}

// Download 简单直接的下载方式
func (sd *SimpleDownloader) Download() error {
	fmt.Printf("🚀 开始下载: %s\n", sd.url)
	startTime := time.Now()

	// 创建请求
	req, err := http.NewRequest("GET", sd.url, nil)
	if err != nil {
		return fmt.Errorf("创建请求失败: %v", err)
	}

	// 设置常用的请求头
	req.Header.Set("User-Agent", "curl/7.68.0") // 模拟curl
	req.Header.Set("Accept", "*/*")

	// 发送请求
	resp, err := sd.client.Do(req)
	if err != nil {
		return fmt.Errorf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP错误: %d %s", resp.StatusCode, resp.Status)
	}

	// 创建目录
	dir := filepath.Dir(sd.path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("创建目录失败: %v", err)
	}

	// 创建文件
	file, err := os.Create(sd.path)
	if err != nil {
		return fmt.Errorf("创建文件失败: %v", err)
	}
	defer file.Close()

	// 获取文件大小
	fileSize := resp.ContentLength
	if fileSize > 0 {
		fmt.Printf("📄 文件大小: %.2f MB\n", float64(fileSize)/1024/1024)
	}

	// 使用大缓冲区直接复制 - 类似curl的方式
	bufferSize := 2 * 1024 * 1024 // 2MB缓冲区
	buffer := make([]byte, bufferSize)

	var downloaded int64
	lastPrintTime := time.Now()
	lastBytes := int64(0)

	for {
		n, err := resp.Body.Read(buffer)
		if n > 0 {
			// 写入文件
			written, writeErr := file.Write(buffer[:n])
			if writeErr != nil {
				return fmt.Errorf("写入文件失败: %v", writeErr)
			}
			if written != n {
				return fmt.Errorf("写入不完整")
			}

			downloaded += int64(n)

			// 每秒打印一次进度
			now := time.Now()
			if now.Sub(lastPrintTime) >= time.Second {
				elapsed := now.Sub(lastPrintTime).Seconds()
				speed := float64(downloaded-lastBytes) / elapsed / 1024 / 1024

				if fileSize > 0 {
					progress := float64(downloaded) / float64(fileSize) * 100
					fmt.Printf("\r📥 进度: %.1f%% (%.1f/%.1fMB) 速度: %.2fMB/s     ",
						progress,
						float64(downloaded)/1024/1024,
						float64(fileSize)/1024/1024,
						speed)
				} else {
					fmt.Printf("\r📥 已下载: %.1fMB 速度: %.2fMB/s     ",
						float64(downloaded)/1024/1024,
						speed)
				}

				lastPrintTime = now
				lastBytes = downloaded
			}
		}

		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("读取数据失败: %v", err)
		}
	}

	// 完成后强制同步
	file.Sync()

	duration := time.Since(startTime)
	avgSpeed := float64(downloaded) / duration.Seconds() / 1024 / 1024

	fmt.Printf("\n✅ 下载完成! 大小: %.2fMB, 用时: %v, 平均速度: %.2fMB/s\n",
		float64(downloaded)/1024/1024,
		duration.Truncate(time.Second),
		avgSpeed)

	return nil
}