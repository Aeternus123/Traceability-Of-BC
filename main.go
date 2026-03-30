package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// 嵌入web目录下的所有静态文件
//
//go:embed images/*
//go:embed web/*
var staticFiles embed.FS

// 缓存配置 - 与前端保持一致
const (
	BATCH_LIST_CACHE_TTL = 5 * time.Minute         // 批次列表缓存5分钟
	QUERY_CACHE_TTL      = 10 * time.Minute        // 查询结果缓存10分钟
	QRCODE_CACHE_TTL     = 30 * time.Minute        // QR码缓存30分钟
	MAX_CACHE_SIZE       = 100                     // 最大缓存条目数
	CLEANUP_INTERVAL     = 2 * time.Minute         // 清理间隔2分钟
	LOCAL_SERVER_ADDR    = ":5500"                 // 本地服务器地址
	LOCAL_WEB_URL        = "http://localhost:5500" // 本地网页访问地址
	YOLO_SERVER_URL      = "http://localhost:8000" // 本地YOLO服务器地址

	// P2P网络配置
	P2P_MAIN_NODE_PORT  = 3000            // 主节点P2P端口
	P2P_SYNC_INTERVAL   = 24 * time.Hour  // 区块链同步间隔(设置为24小时，确保只在启动时同步一次)
	P2P_RECONNECT_DELAY = 5 * time.Second // 重连延迟
)

// 缓存条目结构
type CacheEntry struct {
	Data      interface{}
	Timestamp time.Time
	TTL       time.Duration
}

// 缓存管理器
type CacheManager struct {
	batchList   *CacheEntry
	queryCache  map[string]*CacheEntry
	qrcodeCache map[string]*CacheEntry
	stats       struct {
		hits   int
		misses int
		size   int
	}
	mutex sync.RWMutex
}

// 创建新的缓存管理器
func NewCacheManager() *CacheManager {
	cm := &CacheManager{
		queryCache:  make(map[string]*CacheEntry),
		qrcodeCache: make(map[string]*CacheEntry),
	}

	// 启动清理协程
	go cm.cleanupRoutine()

	return cm
}

// 检查缓存是否有效
func (cm *CacheManager) isValid(entry *CacheEntry) bool {
	if entry == nil {
		return false
	}
	return time.Since(entry.Timestamp) < entry.TTL
}

// 获取缓存
func (cm *CacheManager) get(key, cacheType string) (interface{}, bool) {
	cm.mutex.RLock()
	defer cm.mutex.RUnlock()

	var entry *CacheEntry
	var exists bool

	switch cacheType {
	case "batchList":
		entry = cm.batchList
	case "query":
		entry, exists = cm.queryCache[key]
		if !exists {
			cm.stats.misses++
			return nil, false
		}
	case "qrcode":
		entry, exists = cm.qrcodeCache[key]
		if !exists {
			cm.stats.misses++
			return nil, false
		}
	}

	if cm.isValid(entry) {
		cm.stats.hits++
		return entry.Data, true
	}

	// 缓存过期，删除
	if exists {
		switch cacheType {
		case "query":
			delete(cm.queryCache, key)
		case "qrcode":
			delete(cm.qrcodeCache, key)
		}
		cm.stats.size--
	}
	cm.stats.misses++
	return nil, false
}

// 设置缓存
func (cm *CacheManager) set(key string, data interface{}, ttl time.Duration, cacheType string) {
	cm.mutex.Lock()
	defer cm.mutex.Unlock()

	// 检查缓存大小限制
	if cm.stats.size >= MAX_CACHE_SIZE {
		cm.cleanupOldest(cacheType)
	}

	entry := &CacheEntry{
		Data:      data,
		Timestamp: time.Now(),
		TTL:       ttl,
	}

	switch cacheType {
	case "batchList":
		cm.batchList = entry
	case "query":
		cm.queryCache[key] = entry
		cm.stats.size++
	case "qrcode":
		cm.qrcodeCache[key] = entry
		cm.stats.size++
	}
}

// 清理最旧的缓存条目
func (cm *CacheManager) cleanupOldest(cacheType string) {
	var oldestKey string
	var oldestTime time.Time

	switch cacheType {
	case "query":
		for key, entry := range cm.queryCache {
			if oldestKey == "" || entry.Timestamp.Before(oldestTime) {
				oldestKey = key
				oldestTime = entry.Timestamp
			}
		}
		if oldestKey != "" {
			delete(cm.queryCache, oldestKey)
			cm.stats.size--
		}
	case "qrcode":
		for key, entry := range cm.qrcodeCache {
			if oldestKey == "" || entry.Timestamp.Before(oldestTime) {
				oldestKey = key
				oldestTime = entry.Timestamp
			}
		}
		if oldestKey != "" {
			delete(cm.qrcodeCache, oldestKey)
			cm.stats.size--
		}
	}
}

// 清理过期缓存
func (cm *CacheManager) cleanup() {
	cm.mutex.Lock()
	defer cm.mutex.Unlock()

	now := time.Now()

	// 清理查询缓存
	for key, entry := range cm.queryCache {
		if now.Sub(entry.Timestamp) > entry.TTL {
			delete(cm.queryCache, key)
			cm.stats.size--
		}
	}

	// 清理QR码缓存
	for key, entry := range cm.qrcodeCache {
		if now.Sub(entry.Timestamp) > entry.TTL {
			delete(cm.qrcodeCache, key)
			cm.stats.size--
		}
	}

	// 检查批次列表缓存
	if cm.batchList != nil && now.Sub(cm.batchList.Timestamp) > cm.batchList.TTL {
		cm.batchList = nil
	}
}

// 清理协程
func (cm *CacheManager) cleanupRoutine() {
	ticker := time.NewTicker(CLEANUP_INTERVAL)
	defer ticker.Stop()

	for range ticker.C {
		cm.cleanup()
	}
}

// 清除所有缓存
func (cm *CacheManager) clear() {
	cm.mutex.Lock()
	defer cm.mutex.Unlock()

	cm.batchList = nil
	cm.queryCache = make(map[string]*CacheEntry)
	cm.qrcodeCache = make(map[string]*CacheEntry)
	cm.stats.size = 0

	fmt.Println("所有缓存已清除")
}

// 获取缓存统计信息
func (cm *CacheManager) getStats() map[string]interface{} {
	cm.mutex.RLock()
	defer cm.mutex.RUnlock()

	return map[string]interface{}{
		"hits":            cm.stats.hits,
		"misses":          cm.stats.misses,
		"size":            cm.stats.size,
		"batchListValid":  cm.isValid(cm.batchList),
		"queryCacheSize":  len(cm.queryCache),
		"qrcodeCacheSize": len(cm.qrcodeCache),
	}
}

// 全局缓存管理器实例
var cacheManager = NewCacheManager()

// 认证相关结构
type AuthResponse struct {
	Token string `json:"token"`
	User  User   `json:"user"`
}

type User struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Email    string `json:"email"`
	Role     string `json:"role"`
}

// P2P网络相关结构
type NodeInfo struct {
	ID         string `json:"id"`
	Address    string `json:"address"`
	IsMainNode bool   `json:"is_main_node"`
	LastSeen   int64  `json:"last_seen"`
}

// 请求数据结构 - 与前端和服务器保持一致
type RegisterBatchRequest struct {
	BatchID string `json:"batch_id"`
}

type ProcessingRequest struct {
	BatchID     string      `json:"batch_id"`
	Level       int         `json:"level"`
	ProductInfo ProductInfo `json:"product_info"`
}

// 产品信息结构，包含备注和图片哈希
type ProductInfo struct {
	BatchID      string `json:"batch_id"`
	ProcessorID  string `json:"processor_id"`
	ProcessDate  string `json:"process_date"`
	Details      string `json:"details"`
	QualityCheck string `json:"quality_check"`
	Location     string `json:"location"`
	Note         string `json:"note"`       // 备注字段
	ImageHash    string `json:"image_hash"` // 图片哈希字段
}

// 响应数据结构 - 与服务器端保持一致
type RegisterResponse struct {
	Success bool   `json:"success"`
	BatchID string `json:"batch_id"`
	Message string `json:"message"`
}

type ProcessingResponse struct {
	Success    bool   `json:"success"`
	BatchID    string `json:"batch_id"`
	BlockIndex int    `json:"block_index"`
	BlockHash  string `json:"block_hash"`
	PrevHash   string `json:"prev_hash"`
}

type BatchInfo struct {
	BatchID      string    `json:"batch_id"`
	ProductType  string    `json:"product_type"`
	CreationDate time.Time `json:"creation_date"`
	CurrentLevel int       `json:"current_level"`
	CurrentHash  string    `json:"current_hash"`
	Complete     bool      `json:"complete"`
}

type Blockchain struct {
	BatchID string  `json:"batch_id"`
	Chain   []Block `json:"chain"`
}

// 区块结构，包含备注和图片哈希
type Block struct {
	Index        int         `json:"index"`
	Timestamp    int64       `json:"timestamp"`
	Level        int         `json:"level"`
	ProductInfo  ProductInfo `json:"product_info"`
	PreviousHash string      `json:"previous_hash"`
	CurrentHash  string      `json:"current_hash"`
	Note         string      `json:"note"`       // 备注字段
	ImageHash    string      `json:"image_hash"` // 图片哈希字段
}

type YoloDetectResponse struct {
	Success           bool            `json:"success"`
	Message           string          `json:"message"`
	Detections        []YoloDetection `json:"detections"`
	DetectedImagePath string          `json:"detected_image_path"`
}

type YoloDetection struct {
	Class      string    `json:"class"`
	Confidence float64   `json:"confidence"`
	BBox       []float64 `json:"bbox"` // [x_min, y_min, x_max, y_max]
}

// 主节点配置结构体
type MainNodeConfig struct {
	URL            string  // 主节点HTTP URL
	P2PPort        int     // 主节点P2P端口
	Weight         float64 // 节点权重，用于负载均衡
	NetworkLatency float64 // 网络延迟，用于网络选择
	Active         bool    // 节点是否可用
}

// 所有可用的主节点配置
var mainNodes = []*MainNodeConfig{
	{URL: "http://111.230.7.188:8090", P2PPort: 3000, Weight: 1.0, Active: true},
	// 可以添加更多主节点配置
	{URL: "http://152.136.16.231:8080", P2PPort: 3000, Weight: 1.0, Active: true},
	// {URL: "http://111.230.7.190:8080", P2PPort: 3000, Weight: 1.0, Active: true},
}

// 当前选中的主节点
var currentMainNode *MainNodeConfig

// API端点
const (
	registerEndpoint     = "/api/batch/register"
	addEndpoint          = "/api/process"
	queryEndpoint        = "/product/"
	simpleQueryEndpoint  = "/api/product/simple/"
	qrcodeEndpoint       = "/qrcode/"
	listBatchesEndpoint  = "/api/batches"
	yoloDetectEndpoint   = "/api/detect"
	loginEndpoint        = "/api/auth/login"
	registerUserEndpoint = "/api/auth/register"
	imageUploadEndpoint  = "/api/upload/image" // 图片上传端点
)

// 全局认证令牌
var authToken string
var currentUser User

// 选择策略类型
type SelectionStrategy string

const (
	// 按网络延迟选择主节点
	StrategyNetwork SelectionStrategy = "network"
	// 按权重负载均衡选择主节点
	StrategyTraffic SelectionStrategy = "traffic"
)

// 当前选择策略
var currentSelectionStrategy = StrategyNetwork

// 获取当前主节点的URL
func getServerURL() string {
	if currentMainNode != nil {
		return currentMainNode.URL
	}
	return ""
}

// 获取当前主节点的P2P地址
func getMainNodeP2PAddress() string {
	if currentMainNode != nil {
		// 提取IP地址部分，格式化为P2P地址
		baseURL := strings.Replace(currentMainNode.URL, "http://", "", 1)
		// 提取IP地址（去掉URL中的端口部分）
		ip := strings.Split(baseURL, ":")[0]
		return fmt.Sprintf("%s:%d", ip, currentMainNode.P2PPort)
	}
	return ""
}

// 初始化主节点选择
func initMainNodeSelection() {
	// 默认选择第一个主节点
	if len(mainNodes) > 0 {
		currentMainNode = mainNodes[0]
	}

	// 定期更新网络延迟
	go updateNetworkLatencies()
}

// 选择一个主节点，基于指定策略（ai协助开发@deepseek）
func selectMainNode(strategy SelectionStrategy) *MainNodeConfig {
	// 过滤出可用的主节点
	availableNodes := []*MainNodeConfig{}
	for _, node := range mainNodes {
		if node.Active {
			availableNodes = append(availableNodes, node)
		}
	}

	if len(availableNodes) == 0 {
		return nil
	}

	// 根据策略选择主节点
	switch strategy {
	case StrategyNetwork:
		// 选择网络延迟最低的节点
		bestNode := availableNodes[0]
		for _, node := range availableNodes[1:] {
			if node.NetworkLatency < bestNode.NetworkLatency {
				bestNode = node
			}
		}
		return bestNode
	case StrategyTraffic:
		// 基于权重的负载均衡选择
		// 计算总权重
		totalWeight := 0.0
		for _, node := range availableNodes {
			totalWeight += node.Weight
		}

		// 随机选择一个节点，概率基于权重
		random := rand.Float64() * totalWeight
		currentWeight := 0.0
		for _, node := range availableNodes {
			currentWeight += node.Weight
			if random <= currentWeight {
				return node
			}
		}
		return availableNodes[0] // 默认返回第一个
	default:
		return availableNodes[0] // 默认返回第一个
	}
}

// 更新所有主节点的网络延迟
func updateNetworkLatencies() {
	ticker := time.NewTicker(30 * time.Second) // 每30秒更新一次
	defer ticker.Stop()

	for range ticker.C {
		for _, node := range mainNodes {
			if node.Active {
				// 测量网络延迟
				startTime := time.Now()
				client := http.Client{Timeout: 5 * time.Second}
				resp, err := client.Get(node.URL + "/api/ping")

				if err == nil && resp.StatusCode == http.StatusOK {
					node.NetworkLatency = time.Since(startTime).Seconds()
					resp.Body.Close()
				} else {
					// 如果无法连接，标记为高延迟
					node.NetworkLatency = 1000.0 // 1000秒，表示高延迟
				}
			}
		}
	}
}

// P2P网络相关全局变量
var (
	p2pCurrentNodeID   string            // 当前节点ID
	p2pMainNodeAddress string            // 主节点地址
	p2pConnection      net.Conn          // 与主节点的连接
	p2pIsConnected     bool              // 是否已连接到主节点
	p2pMutex           sync.RWMutex      // 保护P2P相关变量的互斥锁
	syncChan           = make(chan bool) // 同步信号通道
	stopChan           = make(chan bool) // 停止信号通道
	mainNodeMutex      sync.RWMutex      // 保护主节点选择的互斥锁
)

// 打开系统默认浏览器
func openBrowser(url string) error {
	var cmd string
	var args []string

	switch runtime.GOOS {
	case "windows":
		cmd = "cmd"
		args = []string{"/c", "start"}
	case "darwin": // macOS
		cmd = "open"
	case "linux":
		cmd = "xdg-open"
	default:
		return fmt.Errorf("不支持的操作系统: %s", runtime.GOOS)
	}
	args = append(args, url)
	return exec.Command(cmd, args...).Start()
}

// 启动HTTP服务器
func startHTTPServer() {
	// 注册同步API端点
	http.HandleFunc("/api/sync/pruned", handlePrunedSyncRequest)

	// 注册轻节点同步API端点（用于测试）
	http.HandleFunc("/api/sync/light", handleLightSyncRequest)

	// 注册全节点同步API端点
	http.HandleFunc("/api/sync/full", handleFullSyncRequest)
	// 注册静态文件处理器 - 为web目录和images目录分别设置路由
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// 处理根路径请求，重定向到web目录
		if r.URL.Path == "/" {
			// 使用http.Redirect实现正确的重定向
			http.Redirect(w, r, "/web/index.html", http.StatusFound)
			return
		}

		// 创建一个新的文件服务器，使用整个嵌入的文件系统
		fs := http.FileServer(http.FS(staticFiles))
		fs.ServeHTTP(w, r)
	})

	// 注册API路由，供前端调用
	http.HandleFunc("/api/auth/token", handleGetToken)
	http.HandleFunc("/api/auth/user", handleGetUser)
	http.HandleFunc("/api/batches", handleGetBatches)
	http.HandleFunc("/api/product/", handleGetProduct)
	// 注册检测端点，处理图片检测请求并发送结果到arm.py
	http.HandleFunc("/api/detect", handleDetectionRequest)

	log.Printf("HTTP服务器启动在 %s", LOCAL_WEB_URL)
	if err := http.ListenAndServe(LOCAL_SERVER_ADDR, nil); err != nil {
		log.Printf("HTTP服务器错误: %v", err)
	}
}

// 为API处理函数添加CORS支持
func enableCORS(w http.ResponseWriter, r *http.Request) {
	// 允许所有来源
	w.Header().Set("Access-Control-Allow-Origin", "*")
	// 允许的方法
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	// 允许的头部
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
	// 预检请求的有效期
	w.Header().Set("Access-Control-Max-Age", "86400")
	// 处理OPTIONS请求
	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusOK)
		return
	}
}

// API处理函数 - 获取当前令牌
func handleGetToken(w http.ResponseWriter, r *http.Request) {
	enableCORS(w, r)
	if r.Method == "OPTIONS" {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"token": authToken})
}

// API处理函数 - 获取当前用户信息
func handleGetUser(w http.ResponseWriter, r *http.Request) {
	enableCORS(w, r)
	if r.Method == "OPTIONS" {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(currentUser)
}

// API处理函数 - 获取批次列表
func handleGetBatches(w http.ResponseWriter, r *http.Request) {
	enableCORS(w, r)
	if r.Method == "OPTIONS" {
		return
	}
	batches, err := getBatchList()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(batches)
}

// API处理函数 - 获取产品信息
func handleGetProduct(w http.ResponseWriter, r *http.Request) {
	enableCORS(w, r)
	if r.Method == "OPTIONS" {
		return
	}
	batchID := strings.TrimPrefix(r.URL.Path, "/api/product/")
	if batchID == "" {
		http.Error(w, "批次ID不能为空", http.StatusBadRequest)
		return
	}

	simple := r.URL.Query().Get("simple") == "true"
	cacheKey := queryEndpoint + batchID
	if simple {
		cacheKey = simpleQueryEndpoint + batchID
	}

	if data, ok := cacheManager.get(cacheKey, "query"); ok {
		if blockchain, ok := data.(Blockchain); ok {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(blockchain)
			return
		}
	}

	endpoint := queryEndpoint + batchID
	if simple {
		endpoint = simpleQueryEndpoint + batchID
	}

	// 使用sendRequest自动选择主节点发送请求
	resp, err := sendRequest("GET", endpoint, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		http.Error(w, string(body), resp.StatusCode)
		return
	}

	var blockchain Blockchain
	if err := json.NewDecoder(resp.Body).Decode(&blockchain); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	cacheManager.set(cacheKey, blockchain, QUERY_CACHE_TTL, "query")

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(blockchain)
}

// ------------------------------- P2P网络功能 --------------------------------

// 生成P2P节点ID
func generateP2PNodeID() string {
	ip := getLocalIP()
	h := sha256.New()
	h.Write([]byte(fmt.Sprintf("%s:%d:%d", ip, time.Now().Unix(), os.Getpid())))
	return fmt.Sprintf("client_%x", h.Sum(nil)[:6])
}

// 获取本地IP地址
func getLocalIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "127.0.0.1"
	}
	for _, address := range addrs {
		// 检查IP地址是否是IPv4且不是回环地址
		if ipnet, ok := address.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				return ipnet.IP.String()
			}
		}
	}
	return "127.0.0.1"
}

// 初始化P2P网络（ai协助开发@deepseek）
func initP2PNetwork() {
	// 生成节点ID
	p2pCurrentNodeID = generateP2PNodeID()
	// 设置主节点地址
	p2pMainNodeAddress = getMainNodeP2PAddress()
	fmt.Printf("P2P网络初始化 - 节点ID: %s, 主节点地址: %s\n", p2pCurrentNodeID, p2pMainNodeAddress)

	// 启动P2P连接管理协程
	go p2pConnectionManager()

	// 启动区块链同步协程
	go blockchainSyncRoutine()
}

// P2P连接管理协程
func p2pConnectionManager() {
	for {
		select {
		case <-stopChan:
			fmt.Println("P2P连接管理器已停止")
			return
		default:
			if !p2pIsConnected {
				// 尝试连接到主节点
				if err := connectToMainNode(); err != nil {
					//fmt.Printf("连接主节点失败: %v\n", err)
					// 重连延迟
					time.Sleep(P2P_RECONNECT_DELAY)
					continue
				}
			}
			time.Sleep(time.Second)
		}
	}
}

// 连接到主节点（ai协助开发@deepseek）
func connectToMainNode() error {
	// 连接到主节点的P2P端口
	conn, err := net.Dial("tcp", p2pMainNodeAddress)
	if err != nil {
		return fmt.Errorf("无法连接到主节点: %v", err)
	}

	// 创建握手消息
	handshakeMsg := map[string]interface{}{
		"type":         "node_handshake",
		"node_id":      p2pCurrentNodeID,
		"address":      fmt.Sprintf("%s:0", getLocalIP()), // 客户端不需要监听端口
		"is_main_node": false,
	}

	// 发送握手消息
	e := json.NewEncoder(conn)
	if err := e.Encode(handshakeMsg); err != nil {
		conn.Close()
		return fmt.Errorf("发送握手消息失败: %v", err)
	}

	// 读取握手响应
	d := json.NewDecoder(conn)
	var response map[string]interface{}
	if err := d.Decode(&response); err != nil {
		conn.Close()
		return fmt.Errorf("读取握手响应失败: %v", err)
	}

	// 检查握手是否成功
	if _, ok := response["type"]; !ok || response["type"].(string) != "handshake_response" {
		conn.Close()
		return fmt.Errorf("无效的握手响应")
	}

	// 更新连接状态
	p2pMutex.Lock()
	p2pConnection = conn
	p2pIsConnected = true
	p2pMutex.Unlock()

	fmt.Printf("成功连接到主节点 - 节点ID: %s\n", response["node_id"])

	// 启动消息处理协程
	go handleP2PMessages(conn)

	// 如果握手响应中包含区块链概览信息，立即同步
	if overview, ok := response["blockchain_overview"]; ok {
		handleBlockchainOverview(overview)
	}

	return nil
}

// 处理从主节点接收的消息
func handleP2PMessages(conn net.Conn) {
	d := json.NewDecoder(conn)

	for {
		var msg map[string]interface{}
		if err := d.Decode(&msg); err != nil {
			// 连接关闭或出现错误
			p2pMutex.Lock()
			p2pIsConnected = false
			p2pConnection = nil
			p2pMutex.Unlock()
			fmt.Println("与主节点的连接已断开")
			return
		}

		// 根据消息类型处理
		msgType, ok := msg["type"].(string)
		if !ok {
			fmt.Println("无效的P2P消息: 缺少type字段")
			continue
		}

		// 处理不同类型的消息
		handleP2PMessage(msgType, msg)
	}
}

// 处理不同类型的P2P消息
func handleP2PMessage(msgType string, msg map[string]interface{}) {
	switch msgType {
	case "new_block":
		// 收到新区块通知，触发同步
		fmt.Printf("收到新区块通知 - 批次: %s\n", msg["batch_id"])
		syncChan <- true
	case "blockchain_sync_response":
		// 处理区块链同步响应
		handleBlockchainSyncResponse(nil, msg)
	default:
		fmt.Printf("未知的P2P消息类型: %s\n", msgType)
	}
}

// 处理区块链概览信息
func handleBlockchainOverview(overview interface{}) {
	// 这里可以处理从主节点获取的区块链概览信息
	// 例如更新本地缓存的批次列表
	fmt.Println("收到区块链概览信息，准备更新本地数据")
	// 触发同步信号
	syncChan <- true
}

// 区块链同步协程
func blockchainSyncRoutine() {
	ticker := time.NewTicker(P2P_SYNC_INTERVAL)
	defer ticker.Stop()

	for {
		select {
		case <-stopChan:
			fmt.Println("区块链同步协程已停止")
			return
		case <-ticker.C:
			// 定期同步
			syncBlockchain()
		case <-syncChan:
			// 触发同步
			syncBlockchain()
		}
	}
}

// 同步区块链数据 - 默认使用轻节点同步
func syncBlockchain() {
	// 默认执行轻节点同步
	syncBlockchainLight()
}

// 轻节点同步 - 只同步批次列表和最新区块摘要
func syncBlockchainLight() {
	p2pMutex.RLock()
	connected := p2pIsConnected
	conn := p2pConnection
	p2pMutex.RUnlock()

	if !connected || conn == nil {
		return
	}

	fmt.Println("开始轻节点同步...")

	// 发送轻节点同步请求
	syncRequest := map[string]interface{}{
		"type":           "blockchain_light_sync_request",
		"node_id":        p2pCurrentNodeID,
		"is_pruned_sync": false,
	}

	// 发送同步请求
	e := json.NewEncoder(conn)
	if err := e.Encode(syncRequest); err != nil {
		fmt.Printf("发送轻节点同步请求失败: %v\n", err)
		return
	}

	fmt.Println("轻节点同步请求已发送")
}

// 剪枝节点同步 - 同步所有批次的完整区块链数据
func syncBlockchainPruned() {
	p2pMutex.RLock()
	connected := p2pIsConnected
	conn := p2pConnection
	p2pMutex.RUnlock()

	if !connected || conn == nil {
		return
	}

	fmt.Println("开始剪枝节点同步...")

	// 获取当前批次列表
	batches, err := getBatchList()
	if err != nil {
		fmt.Printf("获取批次列表失败: %v\n", err)
		return
	}

	// 对每个批次请求最新的区块数据
	for _, batch := range batches {
		// 创建同步请求消息
		syncRequest := map[string]interface{}{
			"type":           "blockchain_sync_request",
			"batch_id":       batch.BatchID,
			"start_index":    0, // 请求所有区块
			"is_pruned_sync": true,
		}

		// 发送同步请求
		e := json.NewEncoder(conn)
		if err := e.Encode(syncRequest); err != nil {
			fmt.Printf("发送同步请求失败: %v\n", err)
			return
		}

		// 注意：响应处理在handleP2PMessage函数中
		time.Sleep(100 * time.Millisecond) // 避免请求过于频繁
	}

	fmt.Println("剪枝节点同步完成")
}

// 处理剪枝节点同步请求
func handlePrunedSyncRequest(w http.ResponseWriter, r *http.Request) {
	enableCORS(w, r)
	if r.Method == "OPTIONS" {
		return
	}
	// 验证请求方法
	if r.Method != http.MethodPost {
		http.Error(w, "只支持POST请求", http.StatusMethodNotAllowed)
		return
	}

	// 检查认证状态
	if authToken == "" {
		// 尝试重新登录获取令牌
		fmt.Println("未授权访问，尝试重新登录...")
		if !loginPrompt() {
			http.Error(w, "未授权访问", http.StatusUnauthorized)
			return
		}
	}

	// 异步执行剪枝节点同步，避免阻塞HTTP响应
	go func() {
		// 打印当前使用的主节点信息和令牌状态
		fmt.Printf("正在使用主节点: %s\n", getServerURL())
		fmt.Printf("当前令牌状态: %s\n", authToken[:20]+"...") // 只显示部分令牌以保护安全

		// 尝试通过HTTP直接调用/api/sync/pruned端点
		fmt.Println("尝试直接通过HTTP调用剪枝同步端点...")
		endpoint := "/api/sync/pruned"
		serverURL := getServerURL()
		fullURL := serverURL + endpoint

		// 创建请求体（如果需要）
		reqBody, _ := json.Marshal(map[string]interface{}{})

		// 创建带认证的HTTP请求
		req, err := http.NewRequest("POST", fullURL, bytes.NewBuffer(reqBody))
		if err != nil {
			fmt.Printf("创建HTTP请求失败: %v\n", err)
			return
		}

		// 添加Authorization头
		req.Header.Set("Authorization", "Bearer "+authToken)
		req.Header.Set("Content-Type", "application/json")

		fmt.Printf("发送HTTP请求到: %s\n", fullURL)
		fmt.Printf("请求头: Authorization: Bearer %s...\n", authToken[:20])

		// 发送请求
		client := &http.Client{}
		resp, err := client.Do(req)
		if err != nil {
			fmt.Printf("HTTP请求失败: %v\n", err)
		}

		// 处理HTTP响应
		if resp != nil {
			fmt.Printf("HTTP响应状态码: %d\n", resp.StatusCode)

			// 读取响应体以获取更多错误信息
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			fmt.Printf("HTTP响应体: %s\n", string(body))

			// 根据状态码处理
			switch resp.StatusCode {
			case http.StatusUnauthorized:
				fmt.Println("HTTP 401错误: 令牌无效或已过期")
				// 尝试重新登录
				if loginPrompt() {
					fmt.Println("重新登录成功，尝试再次发送请求...")
					// 重新创建请求并添加新令牌
					req, _ := http.NewRequest("POST", fullURL, bytes.NewBuffer(reqBody))
					req.Header.Set("Authorization", "Bearer "+authToken)
					req.Header.Set("Content-Type", "application/json")
					resp, _ := client.Do(req)
					if resp != nil {
						fmt.Printf("重新发送请求后状态码: %d\n", resp.StatusCode)
						resp.Body.Close()
					}
				}
			case http.StatusNotFound:
				fmt.Println("HTTP 404错误: 端点不存在")
				fmt.Println("可能的原因: 1) 服务器上未配置该端点 2) 路径错误")
			default:
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					fmt.Println("HTTP请求成功")
				} else {
					fmt.Printf("HTTP请求返回非成功状态码: %d\n", resp.StatusCode)
				}
			}
		}

		// 无论HTTP请求结果如何，也执行P2P同步作为备选
		fmt.Println("同时执行P2P剪枝节点同步...")
		syncBlockchainPruned()
	}()

	// 返回成功响应
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":        true,
		"message":        "剪枝节点同步已开始",
		"server_url":     getServerURL(),
		"endpoint":       "/api/sync/pruned",
		"token_validity": authToken != "",
	})
}

// 处理轻节点同步请求
func handleLightSyncRequest(w http.ResponseWriter, r *http.Request) {
	enableCORS(w, r)
	if r.Method == "OPTIONS" {
		return
	}
	// 验证请求方法
	if r.Method != http.MethodPost {
		http.Error(w, "只支持POST请求", http.StatusMethodNotAllowed)
		return
	}

	// 检查认证状态
	if authToken == "" {
		http.Error(w, "未授权访问", http.StatusUnauthorized)
		return
	}

	// 异步执行轻节点同步，避免阻塞HTTP响应
	go func() {
		syncBlockchainLight()
	}()

	// 返回成功响应
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "轻节点同步已开始",
	})
}

// 全节点同步 - 同步所有批次的完整区块链数据（包括所有产品信息）
func syncBlockchainFull() {
	p2pMutex.RLock()
	connected := p2pIsConnected
	conn := p2pConnection
	p2pMutex.RUnlock()

	if !connected || conn == nil {
		return
	}

	fmt.Println("开始全节点同步...")

	// 发送全节点同步请求
	syncRequest := map[string]interface{}{
		"type":    "blockchain_full_sync_request",
		"node_id": p2pCurrentNodeID,
	}

	// 发送同步请求
	e := json.NewEncoder(conn)
	if err := e.Encode(syncRequest); err != nil {
		fmt.Printf("发送全节点同步请求失败: %v\n", err)
		return
	}

	fmt.Println("全节点同步请求已发送")
}

// 处理全节点同步请求
func handleFullSyncRequest(w http.ResponseWriter, r *http.Request) {
	enableCORS(w, r)
	if r.Method == "OPTIONS" {
		return
	}
	// 验证请求方法
	if r.Method != http.MethodPost {
		http.Error(w, "只支持POST请求", http.StatusMethodNotAllowed)
		return
	}

	// 检查认证状态
	if authToken == "" {
		http.Error(w, "未授权访问", http.StatusUnauthorized)
		return
	}

	// 异步执行全节点同步，避免阻塞HTTP响应
	go func() {
		syncBlockchainFull()
	}()

	// 返回成功响应
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "全节点同步已开始，这将会占用大量内存",
	})
}

// 处理区块链同步响应
func handleBlockchainSyncResponse(conn net.Conn, msg map[string]interface{}) {
	// 安全地提取成功标志
	var success bool
	if successVal, ok := msg["success"].(bool); ok {
		success = successVal
	}

	// 安全地提取消息
	var message string
	if messageVal, ok := msg["message"].(string); ok {
		message = messageVal
	}

	if !success {
		if message != "" {
			fmt.Printf("区块链同步失败: %s\n", message)
		} else {
			fmt.Printf("区块链同步失败: 未知错误\n")
		}
		return
	}

	// 安全地提取批次ID
	var batchID string
	if batchIDVal, ok := msg["batchID"].(string); ok {
		batchID = batchIDVal
	} else if batchIDVal, ok := msg["batch_id"].(string); ok {
		batchID = batchIDVal
	}

	// 安全地提取区块数据
	var blocksData []interface{}
	if blocksDataVal, ok := msg["blocks"].([]interface{}); ok {
		blocksData = blocksDataVal
	}

	fmt.Printf("成功同步批次 %s 的区块链数据，共 %d 个区块\n", batchID, len(blocksData))

	// 这里可以根据需要处理同步的区块数据
	// 例如更新本地缓存、验证区块链完整性等
	// 由于客户端主要通过HTTP API获取数据，这里主要是保持本地缓存的一致性
	cacheManager.set(queryEndpoint+batchID, nil, 0, "query")
}

func main() {
	// 注册退出信号处理
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		fmt.Println("\n接收到退出信号，正在关闭程序...")
		// 关闭P2P网络连接
		close(stopChan)
		p2pMutex.Lock()
		if p2pConnection != nil {
			p2pConnection.Close()
		}
		p2pMutex.Unlock()
		time.Sleep(500 * time.Millisecond)
		os.Exit(0)
	}()

	// 检查是否以服务模式运行（通过命令行参数判断）
	runAsService := false
	if len(os.Args) > 1 && os.Args[1] == "--service" {
		runAsService = true
		fmt.Println("以服务模式启动，不启用命令行交互...")
	}

	// 初始化主节点选择
	initMainNodeSelection()

	// 启动HTTP服务器（在goroutine中）
	go startHTTPServer()

	// 初始化P2P网络
	initP2PNetwork()

	// 等待服务器启动
	time.Sleep(500 * time.Millisecond)

	// 非服务模式下才自动打开浏览器
	if !runAsService {
		// 自动打开浏览器，直接进入登录页面
		fmt.Println("正在打开浏览器...")
		loginURL := LOCAL_WEB_URL + "/web/login.html"
		if err := openBrowser(loginURL); err != nil {
			fmt.Printf("打开浏览器失败: %v\n", err)
		}

		logout()
	}

	// 服务模式下直接等待，不启动命令行交互
	if runAsService {
		// 创建一个阻塞的通道，保持程序运行
		blockChan := make(chan struct{})
		<-blockChan
		return
	}

	fmt.Println("=== 区块链溯源客户端 ===")

	// 尝试从本地加载保存的令牌
	loadAuthToken()

	// 如果没有令牌或令牌无效，提示登录
	if authToken == "" || !validateToken() {
		fmt.Println("请先登录以使用系统功能")
		if !loginPrompt() {
			fmt.Println("登录失败，但HTTP服务器将继续运行")
			fmt.Println("请通过浏览器访问系统，或在终端输入'login'命令重新尝试登录")
		}
	} else {
		fmt.Printf("已登录: %s (角色: %s)\n", currentUser.Username, currentUser.Role)
	}

	fmt.Println("输入命令 (register/add/query/list/qrcode/yolo/help/exit/logout):")

	reader := bufio.NewReader(os.Stdin)

	for {
		fmt.Print("> ")
		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(input)

		parts := strings.Split(input, " ")
		command := parts[0]

		switch command {
		case "register":
			handleRegisterCommand(reader, parts)
		case "add":
			handleAddCommand(reader, parts)
		case "query":
			handleQueryCommand(parts)
		case "list":
			handleListCommand()
		case "qrcode":
			handleQRCodeCommand(parts)
		case "yolo":
			handleYoloCommand(parts)
		case "cache":
			handleCacheCommand(parts)
		case "login":
			loginPrompt()
		case "logout":
			logout()
			return
		case "help":
			printUsage()
		case "exit":
			fmt.Println("退出客户端")
			return
		default:
			fmt.Println("未知命令，输入help查看帮助")
		}
	}
}

// 登录提示
func loginPrompt() bool {
	reader := bufio.NewReader(os.Stdin)

	fmt.Print("用户名: ")
	username, _ := reader.ReadString('\n')
	username = strings.TrimSpace(username)

	fmt.Print("密码: ")
	password, _ := reader.ReadString('\n')
	password = strings.TrimSpace(password)

	return login(username, password)
}

// 执行登录
func login(username, password string) bool {
	reqBody, err := json.Marshal(map[string]string{
		"username": username,
		"password": password,
	})
	if err != nil {
		log.Printf("创建登录请求失败: %v", err)
		return false
	}

	resp, err := sendRequest("POST", loginEndpoint, bytes.NewBuffer(reqBody))
	if err != nil {
		log.Printf("登录请求失败: %v", err)
		return false
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("读取登录响应失败: %v", err)
		return false
	}

	if resp.StatusCode != http.StatusOK {
		fmt.Printf("登录失败: %s\n", string(body))
		return false
	}

	var authResp AuthResponse
	if err := json.Unmarshal(body, &authResp); err != nil {
		log.Printf("解析登录响应失败: %v", err)
		return false
	}

	authToken = authResp.Token
	currentUser = authResp.User

	// 保存令牌
	saveAuthToken()

	fmt.Printf("登录成功，欢迎 %s!\n", currentUser.Username)
	return true
}

// 验证令牌有效性
func validateToken() bool {
	if authToken == "" {
		return false
	}

	// 创建带认证头的临时请求
	client := createAuthClient()
	resp, err := client.Get(getServerURL() + "/api/auth/validate")
	if err != nil || resp.StatusCode != http.StatusOK {
		return false
	}
	defer resp.Body.Close()

	return true
}

// 保存认证令牌到本地
func saveAuthToken() {
	if authToken == "" {
		return
	}

	data, err := json.Marshal(map[string]interface{}{
		"token":  authToken,
		"user":   currentUser,
		"expiry": time.Now().Add(24 * time.Hour).Unix(), // 24小时有效期
	})

	if err != nil {
		log.Printf("保存令牌失败: %v", err)
		return
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		log.Printf("获取用户目录失败: %v", err)
		return
	}

	tokenPath := filepath.Join(homeDir, ".blockchain_client_token")
	if err := os.WriteFile(tokenPath, data, 0600); err != nil {
		log.Printf("写入令牌文件失败: %v", err)
	} else {
		log.Printf("令牌已保存到: %s", tokenPath)
	}
}

// 从本地加载认证令牌
func loadAuthToken() {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return
	}

	tokenPath := filepath.Join(homeDir, ".blockchain_client_token")
	data, err := os.ReadFile(tokenPath)
	if err != nil {
		return
	}

	var tokenData map[string]interface{}
	if err := json.Unmarshal(data, &tokenData); err != nil {
		return
	}

	// 检查是否过期
	expiry, ok := tokenData["expiry"].(float64)
	if !ok || time.Now().Unix() > int64(expiry) {
		os.Remove(tokenPath) // 删除过期令牌
		return
	}

	token, ok := tokenData["token"].(string)
	if !ok {
		return
	}

	authToken = token

	// 解析用户信息
	userData, ok := tokenData["user"].(map[string]interface{})
	if ok {
		currentUser = User{}

		// 安全地获取用户ID
		if id, ok := userData["id"].(string); ok {
			currentUser.ID = id
		}

		// 安全地获取用户名
		if username, ok := userData["username"].(string); ok {
			currentUser.Username = username
		} else {
			currentUser.Username = "未命名用户"
		}

		// 安全地获取电子邮件
		if email, ok := userData["email"].(string); ok {
			currentUser.Email = email
		}

		// 安全地获取角色
		if role, ok := userData["role"].(string); ok {
			currentUser.Role = role
		} else {
			currentUser.Role = "user"
		}
	}
}

// 退出登录
func logout() {
	homeDir, err := os.UserHomeDir()
	if err == nil {
		tokenPath := filepath.Join(homeDir, ".blockchain_client_token")
		os.Remove(tokenPath)
	}

	authToken = ""
	currentUser = User{}
	fmt.Println("已退出登录")
}

// 创建带认证的HTTP客户端
func createAuthClient() *http.Client {
	return &http.Client{
		Transport: &authTransport{
			token: authToken,
			base:  http.DefaultTransport,
		},
	}
}

// 发送HTTP请求，自动选择主节点
func sendRequest(method string, endpoint string, body io.Reader) (*http.Response, error) {
	// 获取当前主节点的URL
	serverURL := getServerURL()
	if serverURL == "" {
		// 如果没有可用主节点，尝试选择一个
		mainNodeMutex.Lock()
		currentMainNode = selectMainNode(currentSelectionStrategy)
		mainNodeMutex.Unlock()
		serverURL = getServerURL()
		if serverURL == "" {
			return nil, fmt.Errorf("没有可用的主节点")
		}
	}

	// 构建完整的请求URL
	url := serverURL + endpoint

	// 创建请求
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}

	// 使用带认证的客户端发送请求
	client := createAuthClient()
	resp, err := client.Do(req)

	// 如果请求失败，尝试切换主节点
	if err != nil || resp != nil && resp.StatusCode >= 500 {
		fmt.Printf("请求失败: %v, 尝试切换主节点...\n", err)

		// 切换到另一个主节点
		mainNodeMutex.Lock()
		newNode := selectMainNode(currentSelectionStrategy)
		if newNode != nil && newNode != currentMainNode {
			currentMainNode = newNode
			fmt.Printf("已切换到主节点: %s\n", currentMainNode.URL)

			// 重新发送请求
			serverURL = getServerURL()
			url = serverURL + endpoint
			req, _ = http.NewRequest(method, url, body)
			resp, err = client.Do(req)
		}
		mainNodeMutex.Unlock()
	}

	// 处理各种HTTP状态码
	if resp != nil {
		// 如果遇到401错误，尝试重新登录
		if resp.StatusCode == http.StatusUnauthorized {
			fmt.Println("令牌无效或已过期，尝试重新登录...")
			if loginPrompt() {
				// 重新创建带新令牌的客户端
				client = createAuthClient()
				// 重新发送请求
				req, _ = http.NewRequest(method, url, body)
				resp, err = client.Do(req)
			}
		} else if resp.StatusCode == http.StatusNotFound {
			// 处理404错误，提示端点不存在
			fmt.Printf("警告: 请求的端点 %s 在服务器上不存在\n", endpoint)
			fmt.Println("请检查服务器配置或API文档")
		} else if resp.StatusCode >= 400 {
			// 处理其他4xx错误
			fmt.Printf("警告: 服务器返回错误状态码: %d\n", resp.StatusCode)
		}
	}

	return resp, err
}

// 带认证的传输层
type authTransport struct {
	token string
	base  http.RoundTripper
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// 复制请求以避免修改原始请求
	newReq := new(http.Request)
	*newReq = *req
	newReq.Header = make(http.Header, len(req.Header))
	for k, s := range req.Header {
		newReq.Header[k] = append([]string(nil), s...)
	}

	// 添加认证头
	if t.token != "" {
		newReq.Header.Set("Authorization", "Bearer "+t.token)
	}

	// 处理OPTIONS请求的CORS问题
	if req.Method == "OPTIONS" {
		// 移除Authorization头，避免预检请求失败
		newReq.Header.Del("Authorization")
	}

	return t.base.RoundTrip(newReq)
}

func handleRegisterCommand(reader *bufio.Reader, parts []string) {
	if len(parts) < 2 {
		fmt.Println("请指定批次ID")
		fmt.Print("批次ID> ")
		batchID, _ := reader.ReadString('\n')
		batchID = strings.TrimSpace(batchID)
		parts = append(parts, batchID)
	}

	batchID := strings.TrimSpace(parts[1])
	registerBatch(batchID)
}

func handleAddCommand(reader *bufio.Reader, parts []string) {
	// 获取批次列表
	batches, err := getBatchList()
	if err != nil {
		log.Printf("获取批次列表失败: %v", err)
		return
	}

	if len(batches) == 0 {
		fmt.Println("没有可用的批次，请先注册一个批次")
		return
	}

	// 显示批次列表供用户选择
	fmt.Println("\n可用批次列表:")
	for i, batch := range batches {
		fmt.Printf("%d. %s (当前级别: %d)\n", i+1, batch.BatchID, batch.CurrentLevel)
	}

	// 让用户选择批次
	fmt.Print("\n请选择批次 (输入序号或直接输入批次ID): ")
	batchInput, _ := reader.ReadString('\n')
	batchInput = strings.TrimSpace(batchInput)

	var selectedBatch BatchInfo
	if idx, err := strconv.Atoi(batchInput); err == nil && idx > 0 && idx <= len(batches) {
		// 用户输入的是有效序号
		selectedBatch = batches[idx-1]
	} else {
		// 用户输入的是批次ID，尝试查找
		found := false
		for _, batch := range batches {
			if batch.BatchID == batchInput {
				selectedBatch = batch
				found = true
				break
			}
		}
		if !found {
			fmt.Printf("未找到批次ID: %s\n", batchInput)
			return
		}
	}

	// 处理JSON文件输入
	if len(parts) < 2 {
		fmt.Println("请指定JSON文件路径")
		fmt.Print("文件路径> ")
		filePath, _ := reader.ReadString('\n')
		filePath = strings.TrimSpace(filePath)
		parts = append(parts, filePath)
	}

	jsonFile := strings.TrimSpace(parts[1])
	// 处理拖放文件可能包含的引号
	jsonFile = strings.Trim(jsonFile, `"'`)

	// 读取JSON文件
	data, err := os.ReadFile(jsonFile)
	if err != nil {
		log.Printf("读取JSON文件失败: %v", err)
		return
	}

	var req ProcessingRequest
	if err := json.Unmarshal(data, &req); err != nil {
		log.Printf("解析JSON失败: %v", err)
		return
	}

	// 新增：获取用户备注
	fmt.Print("请输入处理备注 (可选): ")
	note, _ := reader.ReadString('\n')
	note = strings.TrimSpace(note)

	// 新增：询问是否需要上传图片
	var imageHash string
	fmt.Print("是否需要上传相关图片? (y/n): ")
	imageChoice, _ := reader.ReadString('\n')
	imageChoice = strings.TrimSpace(strings.ToLower(imageChoice))

	if imageChoice == "y" || imageChoice == "yes" {
		fmt.Print("图片文件路径: ")
		imagePath, _ := reader.ReadString('\n')
		imagePath = strings.TrimSpace(imagePath)
		imagePath = strings.Trim(imagePath, `"'`)

		// 上传图片并获取哈希
		hash, err := uploadImageForHash(imagePath)
		if err == nil && hash != "" {
			imageHash = hash
			fmt.Printf("图片上传成功，哈希值: %s\n", imageHash)
		}
	}

	// 设置批次ID、级别、备注和图片哈希
	req.BatchID = selectedBatch.BatchID
	req.Level = selectedBatch.CurrentLevel + 1
	req.ProductInfo.BatchID = selectedBatch.BatchID
	req.ProductInfo.Note = note
	req.ProductInfo.ImageHash = imageHash

	// 发送请求
	jsonData, err := json.Marshal(req)
	if err != nil {
		log.Printf("编码请求数据失败: %v", err)
		return
	}

	resp, err := sendRequest("POST", addEndpoint, bytes.NewBuffer(jsonData))
	if err != nil {
		log.Printf("请求失败: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		log.Printf("服务器返回错误: %s - %s", resp.Status, string(body))
		return
	}

	var result ProcessingResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		log.Printf("解析响应失败: %v", err)
		return
	}

	fmt.Println("处理记录添加成功!")
	fmt.Printf("批次ID: %s\n", result.BatchID)
	fmt.Printf("区块索引: %d\n", result.BlockIndex)
	fmt.Printf("当前哈希: %s\n", result.BlockHash)
	fmt.Printf("前一哈希: %s\n", result.PrevHash)

	// 清除相关缓存
	cacheManager.set(listBatchesEndpoint, nil, 0, "batchList")
	cacheManager.set(queryEndpoint+req.BatchID, nil, 0, "query")
}

// 上传图片并获取哈希的函数
func uploadImageForHash(imagePath string) (string, error) {
	// 检查文件是否存在
	if _, err := os.Stat(imagePath); os.IsNotExist(err) {
		return "", fmt.Errorf("图片文件不存在")
	}

	// 读取图片文件
	imageData, err := os.ReadFile(imagePath)
	if err != nil {
		return "", fmt.Errorf("读取图片失败: %v", err)
	}

	// 创建multipart表单数据
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	// 添加图片文件
	part, err := writer.CreateFormFile("image", filepath.Base(imagePath))
	if err != nil {
		return "", fmt.Errorf("创建表单失败: %v", err)
	}
	part.Write(imageData)
	writer.Close()

	// 发送上传请求
	resp, err := sendRequest("POST", imageUploadEndpoint, &buf)
	if err != nil {
		return "", fmt.Errorf("上传请求失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("上传失败: %s", string(body))
	}

	// 解析响应获取图片哈希
	var result map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("解析响应失败: %v", err)
	}

	return result["hash"], nil
}

func handleQueryCommand(parts []string) {
	if len(parts) < 2 {
		fmt.Println("请指定批次ID")
		return
	}
	batchID := parts[1]

	// 检查是否需要简化查询
	simple := false
	if len(parts) > 2 && parts[2] == "simple" {
		simple = true
	}

	queryProductChain(batchID, simple)
}

func handleListCommand() {
	batches, err := getBatchList()
	if err != nil {
		log.Printf("获取批次列表失败: %v", err)
		return
	}

	fmt.Println("\n批次列表:")
	for _, batch := range batches {
		fmt.Printf("\n批次ID: %s\n", batch.BatchID)
		fmt.Printf("产品类型: %s\n", batch.ProductType)
		fmt.Printf("创建时间: %s\n", batch.CreationDate.Format("2006-01-02 15:04:05"))
		fmt.Printf("当前级别: %d\n", batch.CurrentLevel)
		fmt.Printf("当前哈希: %s\n", batch.CurrentHash)
		fmt.Printf("是否完成: %t\n", batch.Complete)
	}
}

func handleQRCodeCommand(parts []string) {
	if len(parts) < 2 {
		fmt.Println("请指定批次ID")
		return
	}
	batchID := parts[1]
	outputPath := ""
	if len(parts) > 2 {
		outputPath = parts[2]
	}
	downloadQRCode(batchID, outputPath)
}

// 获取批次列表的公共函数
func getBatchList() ([]BatchInfo, error) {
	// 检查缓存
	if data, ok := cacheManager.get(listBatchesEndpoint, "batchList"); ok {
		if batches, ok := data.([]BatchInfo); ok {
			return batches, nil
		}
	}

	resp, err := sendRequest("GET", listBatchesEndpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	// 打印响应状态码和响应体，用于调试
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("服务器返回错误: %s - %s", resp.Status, string(body))
	}

	var batches []BatchInfo
	if err := json.NewDecoder(resp.Body).Decode(&batches); err != nil {
		return nil, fmt.Errorf("解析响应失败: %v", err)
	}

	// 缓存结果
	cacheManager.set(listBatchesEndpoint, batches, BATCH_LIST_CACHE_TTL, "batchList")
	return batches, nil
}

func registerBatch(batchID string) {
	req := RegisterBatchRequest{
		BatchID: batchID,
	}

	jsonData, err := json.Marshal(req)
	if err != nil {
		log.Printf("编码请求数据失败: %v", err)
		return
	}

	resp, err := sendRequest("POST", registerEndpoint, bytes.NewBuffer(jsonData))
	if err != nil {
		log.Printf("请求失败: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		log.Printf("服务器返回错误: %s - %s", resp.Status, string(body))
		return
	}

	var result RegisterResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		log.Printf("解析响应失败: %v", err)
		return
	}

	fmt.Println("批次注册成功!")
	fmt.Printf("批次ID: %s\n", result.BatchID)
	fmt.Printf("消息: %s\n", result.Message)

	// 清除批次列表缓存
	cacheManager.set(listBatchesEndpoint, nil, 0, "batchList")
}

func queryProductChain(batchID string, simple bool) {
	// 检查缓存
	cacheKey := queryEndpoint + batchID
	if simple {
		cacheKey = simpleQueryEndpoint + batchID
	}

	if data, ok := cacheManager.get(cacheKey, "query"); ok {
		if blockchain, ok := data.(Blockchain); ok {
			fmt.Printf("\n批次 %s 的溯源信息 (来自缓存):\n", blockchain.BatchID)
			for _, block := range blockchain.Chain {
				printBlock(block, simple)
			}
			return
		}
	}

	// 选择查询端点
	endpoint := queryEndpoint + batchID
	if simple {
		endpoint = simpleQueryEndpoint + batchID
	}

	// 使用sendRequest自动选择主节点发送请求
	resp, err := sendRequest("GET", endpoint, nil)
	if err != nil {
		log.Printf("查询失败: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		log.Printf("服务器返回错误: %s - %s", resp.Status, string(body))
		return
	}

	var blockchain Blockchain
	if err := json.NewDecoder(resp.Body).Decode(&blockchain); err != nil {
		log.Printf("解析响应失败: %v", err)
		return
	}

	// 缓存结果
	cacheManager.set(cacheKey, blockchain, QUERY_CACHE_TTL, "query")

	fmt.Printf("\n批次 %s 的溯源信息:\n", blockchain.BatchID)
	for _, block := range blockchain.Chain {
		printBlock(block, simple)
	}
}

func printBlock(block Block, simple bool) {
	if simple {
		fmt.Printf("\n[区块 #%d - 级别 %d]\n", block.Index, block.Level)
		fmt.Printf("处理者: %s\n", block.ProductInfo.ProcessorID)
		fmt.Printf("处理日期: %s\n", block.ProductInfo.ProcessDate)
		if block.Note != "" {
			fmt.Printf("备注: %s\n", block.Note)
		}
		if block.ImageHash != "" {
			fmt.Printf("图片哈希: %s\n", block.ImageHash)
		}
		fmt.Printf("当前哈希: %s\n", block.CurrentHash)
	} else {
		fmt.Printf("\n[区块 #%d - 级别 %d]\n", block.Index, block.Level)
		fmt.Printf("批次ID: %s\n", block.ProductInfo.BatchID)
		fmt.Printf("时间戳: %d\n", block.Timestamp)
		fmt.Printf("处理者: %s\n", block.ProductInfo.ProcessorID)
		fmt.Printf("处理日期: %s\n", block.ProductInfo.ProcessDate)
		fmt.Printf("详情: %s\n", block.ProductInfo.Details)
		fmt.Printf("质量检查: %s\n", block.ProductInfo.QualityCheck)
		fmt.Printf("位置: %s\n", block.ProductInfo.Location)
		if block.Note != "" {
			fmt.Printf("备注: %s\n", block.Note)
		}
		if block.ImageHash != "" {
			fmt.Printf("图片哈希: %s\n", block.ImageHash)
			fmt.Printf("图片查看: %s/image/%s\n", getServerURL(), block.ImageHash)
		}
		fmt.Printf("前一哈希: %s\n", block.PreviousHash)
		fmt.Printf("当前哈希: %s\n", block.CurrentHash)
	}
}

func downloadQRCode(batchID, outputPath string) {
	if outputPath == "" {
		outputPath = batchID + ".png"
	}

	// 检查缓存
	if data, ok := cacheManager.get(qrcodeEndpoint+batchID, "qrcode"); ok {
		if qrData, ok := data.([]byte); ok {
			if err := os.WriteFile(outputPath, qrData, 0644); err == nil {
				absPath, _ := filepath.Abs(outputPath)
				fmt.Printf("QR码已保存到: %s (来自缓存)\n", absPath)
				return
			}
		}
	}

	// 使用sendRequest自动选择主节点发送请求
	resp, err := sendRequest("GET", qrcodeEndpoint+batchID, nil)
	if err != nil {
		log.Printf("下载失败: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		log.Printf("服务器返回错误: %s - %s", resp.Status, string(body))
		return
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("读取响应数据失败: %v", err)
		return
	}

	// 缓存QR码
	cacheManager.set(qrcodeEndpoint+batchID, data, QRCODE_CACHE_TTL, "qrcode")

	if err := os.WriteFile(outputPath, data, 0644); err != nil {
		log.Printf("保存QR码失败: %v", err)
		return
	}

	absPath, _ := filepath.Abs(outputPath)
	fmt.Printf("QR码已保存到: %s\n", absPath)
}

func handleCacheCommand(parts []string) {
	if len(parts) < 2 {
		fmt.Println("缓存管理命令:")
		fmt.Println("  cache stats         - 显示缓存统计")
		fmt.Println("  cache clear         - 清除所有缓存")
		fmt.Println("  cache info          - 显示缓存详细信息")
		return
	}

	subCommand := parts[1]
	switch subCommand {
	case "stats":
		stats := cacheManager.getStats()
		fmt.Println("\n=== 缓存统计 ===")
		fmt.Printf("缓存命中: %d\n", stats["hits"])
		fmt.Printf("缓存未命中: %d\n", stats["misses"])
		fmt.Printf("总缓存条目: %d\n", stats["size"])
		fmt.Printf("批次列表缓存: %v\n", stats["batchListValid"])
		fmt.Printf("查询缓存大小: %d\n", stats["queryCacheSize"])
		fmt.Printf("QR码缓存大小: %d\n", stats["qrcodeCacheSize"])

		if stats["hits"].(int)+stats["misses"].(int) > 0 {
			hitRate := float64(stats["hits"].(int)) / float64(stats["hits"].(int)+stats["misses"].(int)) * 100
			fmt.Printf("缓存命中率: %.1f%%\n", hitRate)
		}

	case "clear":
		cacheManager.clear()

	case "info":
		stats := cacheManager.getStats()
		fmt.Println("\n=== 缓存详细信息 ===")
		fmt.Printf("批次列表缓存TTL: %v\n", BATCH_LIST_CACHE_TTL)
		fmt.Printf("查询结果缓存TTL: %v\n", QUERY_CACHE_TTL)
		fmt.Printf("QR码缓存TTL: %v\n", QRCODE_CACHE_TTL)
		fmt.Printf("最大缓存大小: %d\n", MAX_CACHE_SIZE)
		fmt.Printf("清理间隔: %v\n", CLEANUP_INTERVAL)
		fmt.Printf("当前缓存状态:\n")
		fmt.Printf("  - 批次列表: %v\n", stats["batchListValid"])
		fmt.Printf("  - 查询缓存: %d 条\n", stats["queryCacheSize"])
		fmt.Printf("  - QR码缓存: %d 条\n", stats["qrcodeCacheSize"])

	default:
		fmt.Printf("未知的缓存命令: %s\n", subCommand)
		fmt.Println("可用命令: stats, clear, info")
	}
}

// YOLOv8检测命令处理
func handleYoloCommand(parts []string) {
	if len(parts) < 2 {
		fmt.Println("请指定图片文件路径")
		fmt.Print("图片路径> ")
		reader := bufio.NewReader(os.Stdin)
		imagePath, _ := reader.ReadString('\n')
		imagePath = strings.TrimSpace(imagePath)
		parts = append(parts, imagePath)
	}

	imagePath := strings.TrimSpace(parts[1])
	// 处理拖放文件可能包含的引号
	imagePath = strings.Trim(imagePath, `"'`)

	// 检查文件是否存在
	if _, err := os.Stat(imagePath); os.IsNotExist(err) {
		fmt.Printf("图片文件不存在: %s\n", imagePath)
		return
	}

	fmt.Printf("正在使用YOLOv8检测图片: %s\n", imagePath)

	// 调用YOLOv8检测API
	detectImage(imagePath)
}

// 执行YOLOv8检测
func detectImage(imagePath string) {
	// 读取图片文件
	imageData, err := os.ReadFile(imagePath)
	if err != nil {
		fmt.Printf("读取图片文件失败: %v\n", err)
		return
	}

	// 创建multipart表单数据
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	// 添加图片文件
	part, err := writer.CreateFormFile("image", filepath.Base(imagePath))
	if err != nil {
		fmt.Printf("创建表单文件失败: %v\n", err)
		return
	}
	part.Write(imageData)
	writer.Close()

	// 发送POST请求到YOLOv8检测API，使用sendRequest自动选择主节点
	resp, err := sendRequest("POST", yoloDetectEndpoint, &buf)
	if err != nil {
		fmt.Printf("YOLOv8检测请求失败: %v\n", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		fmt.Printf("YOLOv8检测API返回错误: %s - %s\n", resp.Status, string(body))
		return
	}

	var result YoloDetectResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		fmt.Printf("解析YOLOv8检测响应失败: %v\n", err)
		return
	}

	if result.Success {
		fmt.Println("\n=== YOLOv8检测结果 ===")
		fmt.Printf("检测状态: %s\n", result.Message)

		if len(result.Detections) > 0 {
			fmt.Println("\n检测到的对象:")
			for i, detection := range result.Detections {
				fmt.Printf("%d. 类别: %s, 置信度: %.2f%%, 边界框: %v\n",
					i+1, detection.Class, detection.Confidence*100, detection.BBox)
			}
		} else {
			fmt.Println("未检测到任何对象")
		}

		if result.DetectedImagePath != "" {
			fmt.Printf("\n检测结果图片已保存到: %s\n", result.DetectedImagePath)

			// 询问用户是否要上传检测结果
			fmt.Print("\n是否要上传检测结果图片? (y/n): ")
			reader := bufio.NewReader(os.Stdin)
			choice, _ := reader.ReadString('\n')
			choice = strings.TrimSpace(strings.ToLower(choice))

			if choice == "y" || choice == "yes" {
				uploadDetectedImage(result.DetectedImagePath)
			}
		}

		// 将检测结果发送给arm.py
		sendDetectionResultToYoloServer(result)
	} else {
		fmt.Printf("YOLOv8检测失败: %s\n", result.Message)
	}
}

// 将检测结果发送给arm.py (使用HTTP请求)
func sendDetectionResultToYoloServer(result YoloDetectResponse) {
	fmt.Printf("[%s] 开始处理检测结果发送到yoloserver.py\n", time.Now().Format("2006-01-02 15:04:05"))
	fmt.Printf("[%s] 检测结果状态: %v, 检测到 %d 个对象\n", time.Now().Format("2006-01-02 15:04:05"), result.Success, len(result.Detections))
	fmt.Printf("[%s] 根据需求，将发送默认操作信号\n", time.Now().Format("2006-01-02 15:04:05"))

	// 设置HTTP服务器URL
	yoloServerURL := "http://localhost:8000/api/detect"
	fmt.Printf("[%s] 目标URL: %s\n", time.Now().Format("2006-01-02 15:04:05"), yoloServerURL)

	// 准备请求数据 - 始终发送默认操作信号
	var requestData []byte
	var err error

	// 无论检测结果如何，都发送默认操作信号
	fmt.Printf("[%s] 准备发送默认操作信号\n", time.Now().Format("2006-01-02 15:04:05"))
	data := map[string]interface{}{
		"success":           true,
		"detections":        []interface{}{},
		"default_operation": true,
	}
	requestData, err = json.Marshal(data)

	if err != nil {
		fmt.Printf("[%s] 将检测结果转换为JSON失败: %v\n", time.Now().Format("2006-01-02 15:04:05"), err)
		return
	}

	// 发送HTTP POST请求
	fmt.Printf("[%s] 准备发送HTTP POST请求...\n", time.Now().Format("2006-01-02 15:04:05"))
	fmt.Printf("[%s] 请求数据大小: %d 字节\n", time.Now().Format("2006-01-02 15:04:05"), len(requestData))
	resp, err := http.Post(
		yoloServerURL,
		"application/json",
		bytes.NewBuffer(requestData),
	)

	if err != nil {
		fmt.Printf("[%s] 发送HTTP请求失败: %v\n", time.Now().Format("2006-01-02 15:04:05"), err)
		fmt.Printf("[%s] 提示: 请确保yoloserver.py已在后台运行并启动了HTTP服务器\n", time.Now().Format("2006-01-02 15:04:05"))
		fmt.Printf("[%s] 可以通过运行: python yoloserver.py 来启动YOLO检测服务\n", time.Now().Format("2006-01-02 15:04:05"))
		return
	}
	defer resp.Body.Close()

	fmt.Printf("[%s] HTTP请求成功发送，收到响应状态码: %d\n", time.Now().Format("2006-01-02 15:04:05"), resp.StatusCode)

	// 读取响应
	var responseData map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&responseData); err != nil {
		fmt.Printf("[%s] 解析服务器响应失败: %v\n", time.Now().Format("2006-01-02 15:04:05"), err)
	} else {
		status, ok := responseData["status"].(string)
		message, _ := responseData["message"].(string)
		fmt.Printf("[%s] 响应数据: status=%v, message=%v\n", time.Now().Format("2006-01-02 15:04:05"), status, message)
		if ok && status == "success" {
			fmt.Printf("[%s] YOLO检测服务操作成功: %s\n", time.Now().Format("2006-01-02 15:04:05"), message)
		} else {
			fmt.Printf("[%s] YOLO检测服务操作失败: %s\n", time.Now().Format("2006-01-02 15:04:05"), message)
		}
	}

	fmt.Printf("[%s] 检测结果处理请求已完成\n", time.Now().Format("2006-01-02 15:04:05"))
}

// HTTP处理器：处理检测请求
func handleDetectionRequest(w http.ResponseWriter, r *http.Request) {
	fmt.Printf("[%s] 收到检测请求，方法: %s\n", time.Now().Format("2006-01-02 15:04:05"), r.Method)
	fmt.Printf("[%s] 请求来源: %s\n", time.Now().Format("2006-01-02 15:04:05"), r.RemoteAddr)

	enableCORS(w, r)
	if r.Method == "OPTIONS" {
		fmt.Printf("[%s] 处理OPTIONS请求，返回200 OK\n", time.Now().Format("2006-01-02 15:04:05"))
		return
	}

	// 验证请求方法
	if r.Method != http.MethodPost {
		fmt.Printf("[%s] 错误: 不支持的HTTP方法 %s\n", time.Now().Format("2006-01-02 15:04:05"), r.Method)
		http.Error(w, "只支持POST请求", http.StatusMethodNotAllowed)
		return
	}
	fmt.Printf("[%s] 验证通过，开始处理POST请求\n", time.Now().Format("2006-01-02 15:04:05"))

	// 读取图片文件
	fmt.Printf("[%s] 开始读取图片文件...\n", time.Now().Format("2006-01-02 15:04:05"))
	r.ParseMultipartForm(10 << 20) // 10MB
	file, header, err := r.FormFile("image")
	if err != nil {
		fmt.Printf("[%s] 错误: 获取图片文件失败 - %v\n", time.Now().Format("2006-01-02 15:04:05"), err)
		http.Error(w, "获取图片文件失败: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer file.Close()
	fmt.Printf("[%s] 成功获取图片文件: %s\n", time.Now().Format("2006-01-02 15:04:05"), header.Filename)

	// 读取图片数据
	imageData, err := io.ReadAll(file)
	if err != nil {
		fmt.Printf("[%s] 错误: 读取图片失败 - %v\n", time.Now().Format("2006-01-02 15:04:05"), err)
		http.Error(w, "读取图片失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	fmt.Printf("[%s] 成功读取图片数据，大小: %d 字节\n", time.Now().Format("2006-01-02 15:04:05"), len(imageData))

	// 创建multipart表单数据
	fmt.Printf("[%s] 创建multipart表单数据...\n", time.Now().Format("2006-01-02 15:04:05"))
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	// 添加图片文件
	part, err := writer.CreateFormFile("image", "detected_image.jpg")
	if err != nil {
		fmt.Printf("[%s] 错误: 创建表单失败 - %v\n", time.Now().Format("2006-01-02 15:04:05"), err)
		http.Error(w, "创建表单失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	part.Write(imageData)
	writer.Close()
	fmt.Printf("[%s] 成功创建表单数据，准备发送到本地YOLO服务器\n", time.Now().Format("2006-01-02 15:04:05"))

	// 直接发送POST请求到本地YOLO服务器
	yoloURL := YOLO_SERVER_URL + yoloDetectEndpoint
	fmt.Printf("[%s] 发送POST请求到本地YOLO服务器: %s\n", time.Now().Format("2006-01-02 15:04:05"), yoloURL)

	// 创建请求
	req, err := http.NewRequest("POST", yoloURL, &buf)
	if err != nil {
		fmt.Printf("[%s] 错误: 创建请求失败 - %v\n", time.Now().Format("2006-01-02 15:04:05"), err)
		http.Error(w, "创建请求失败: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// 设置内容类型
	req.Header.Set("Content-Type", writer.FormDataContentType())

	// 发送请求
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("[%s] 错误: 检测请求失败 - %v\n", time.Now().Format("2006-01-02 15:04:05"), err)
		http.Error(w, "检测请求失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()
	fmt.Printf("[%s] 收到YOLO服务器响应，状态码: %d\n", time.Now().Format("2006-01-02 15:04:05"), resp.StatusCode)

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		errorMsg := string(body)
		fmt.Printf("[%s] 错误: YOLO服务器返回错误 - %s\n", time.Now().Format("2006-01-02 15:04:05"), errorMsg)
		http.Error(w, "YOLO服务器返回错误: "+errorMsg, resp.StatusCode)
		return
	}

	// 解析检测结果
	fmt.Printf("[%s] 开始解析检测结果...\n", time.Now().Format("2006-01-02 15:04:05"))
	var result YoloDetectResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		fmt.Printf("[%s] 错误: 解析检测结果失败 - %v\n", time.Now().Format("2006-01-02 15:04:05"), err)
		http.Error(w, "解析检测结果失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	fmt.Printf("[%s] 成功解析检测结果，状态: %v，检测到 %d 个对象\n", time.Now().Format("2006-01-02 15:04:05"), result.Success, len(result.Detections))

	// 将检测结果发送给yoloserver.py
	fmt.Printf("[%s] 准备将检测结果发送到yoloserver.py...\n", time.Now().Format("2006-01-02 15:04:05"))
	sendDetectionResultToYoloServer(result)
	fmt.Printf("[%s] 检测结果已发送到yoloserver.py\n", time.Now().Format("2006-01-02 15:04:05"))

	// 返回检测结果给客户端
	fmt.Printf("[%s] 准备返回检测结果给客户端\n", time.Now().Format("2006-01-02 15:04:05"))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// 上传检测结果图片
func uploadDetectedImage(imagePath string) {
	fmt.Printf("正在上传检测结果图片: %s\n", imagePath)

	// 读取图片文件
	imageData, err := os.ReadFile(imagePath)
	if err != nil {
		fmt.Printf("读取检测结果图片失败: %v\n", err)
		return
	}

	// 创建multipart表单数据用于上传
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	// 添加图片文件
	part, err := writer.CreateFormFile("detected_image", filepath.Base(imagePath))
	if err != nil {
		fmt.Printf("创建上传表单失败: %v\n", err)
		return
	}
	part.Write(imageData)
	writer.Close()

	// 发送上传请求，使用sendRequest自动选择主节点
	resp, err := sendRequest("POST", "/api/upload/detected-image", &buf)
	if err != nil {
		fmt.Printf("上传检测结果图片失败: %v\n", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		fmt.Println("检测结果图片上传成功!")
	} else {
		body, _ := io.ReadAll(resp.Body)
		fmt.Printf("上传失败: %s - %s\n", resp.Status, string(body))
	}
}

func printUsage() {
	fmt.Println("\n可用命令:")
	fmt.Println("  register <批次ID>     - 注册新批次")
	fmt.Println("  add <json文件>        - 从JSON文件添加处理记录")
	fmt.Println("  query <批次ID> [simple] - 查询产品溯源链，可选simple参数获取简化信息")
	fmt.Println("  list                 - 列出所有批次")
	fmt.Println("  qrcode <批次ID> [输出路径] - 下载QR码")
	fmt.Println("  yolo <图片路径>       - 使用YOLOv10检测图片")
	fmt.Println("  cache <命令>          - 缓存管理 (stats/clear/info)")
	fmt.Println("  login                - 重新登录")
	fmt.Println("  logout               - 退出登录")
	fmt.Println("  help                 - 显示帮助信息")
	fmt.Println("  exit                 - 退出客户端")
	fmt.Println("\nJSON文件格式示例:")
	fmt.Println(`{
  "batch_id": "batch-2023-08-001",
  "level": 1,
  "product_info": {
    "processor_id": "factory-001",
    "process_date": "2023-06-15",
    "details": "羊毛清洗和梳理",
    "quality_check": "通过",
    "location": "35.6895° N, 139.6917° E"
  }
}`)
	fmt.Println("\n使用提示:")
	fmt.Println("- 使用register命令先注册批次")
	fmt.Println("- 确保JSON中的batch_id与注册的一致")
	fmt.Println("- 可以直接拖放JSON文件到终端窗口")
	fmt.Println("- 文件路径中包含空格时，请用引号括起来")
	fmt.Println("- 添加记录时会自动显示批次列表供选择")
}
