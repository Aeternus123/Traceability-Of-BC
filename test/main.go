package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/dgrijalva/jwt-go"
	"github.com/gorilla/mux"
	_ "github.com/mattn/go-sqlite3"
	"github.com/skip2/go-qrcode"
)

// min 返回两个浮点数中的较小值
func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

// max 返回两个浮点数中的较大值
func max(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// ------------------------------- 数据结构 --------------------------------
type ProductInfo struct {
	BatchID       string `json:"batch_id"`       // 批次/个体唯一标识
	ProcessorID   string `json:"processor_id"`   // 处理者ID
	ProcessDate   string `json:"process_date"`   // 处理日期
	Details       string `json:"details"`        // 处理详情
	QualityCheck  string `json:"quality_check"`  // 质量检测结果
	Location      string `json:"location"`       // 地理位置
	Note          string `json:"note"`           // 用户备注文本
	ImageHash     string `json:"image_hash"`     // 图片哈希值，用于关联图片
	DetectionHash string `json:"detection_hash"` // 检测结果图片哈希
}

// 流程可视化数据结构
type ProcessFlowData struct {
	BatchID      string        `json:"batch_id"`
	ProcessSteps []ProcessStep `json:"process_steps"`
	TotalSteps   int           `json:"total_steps"`
	CurrentLevel int           `json:"current_level"`
	Complete     bool          `json:"complete"`
}

type ProcessStep struct {
	Level         int    `json:"level"`
	Description   string `json:"description"`
	ProcessorID   string `json:"processor_id"`
	ProcessDate   string `json:"process_date"`
	Details       string `json:"details"`
	QualityCheck  string `json:"quality_check"`
	Location      string `json:"location"`
	Timestamp     int64  `json:"timestamp"`
	BlockHash     string `json:"block_hash"`
	Note          string `json:"note"`           // 步骤备注
	ImageHash     string `json:"image_hash"`     // 步骤图片哈希
	DetectionHash string `json:"detection_hash"` // 检测结果图片哈希
}

// 区块头结构 - 用于轻节点同步
type BlockHeader struct {
	Index        int    `json:"index"`
	Timestamp    int64  `json:"timestamp"`
	Level        int    `json:"level"`
	PreviousHash string `json:"previous_hash"`
	CurrentHash  string `json:"current_hash"`
	MerkleRoot   string `json:"merkle_root"`
}

type Block struct {
	Index        int         `json:"index"`
	Timestamp    int64       `json:"timestamp"`
	Level        int         `json:"level"` // 0=来源, 1=一级加工厂, ...
	ProductInfo  ProductInfo `json:"product_info"`
	PreviousHash string      `json:"previous_hash"`
	CurrentHash  string      `json:"current_hash"`
	MerkleRoot   string      `json:"merkle_root"` // 默克尔树根，用于数据验证
}

// 节点信息结构 - 用于P2P网络
type NodeInfo struct {
	ID         string `json:"id"`
	Address    string `json:"address"`
	IsMainNode bool   `json:"is_main_node"`
	LastSeen   int64  `json:"last_seen"`
}

// 节点负载信息结构 - 用于客户端负载均衡
type NodeLoadInfo struct {
	CPUUsage        float64 `json:"cpu_usage"`         // CPU使用率
	MemoryUsage     float64 `json:"memory_usage"`      // 内存使用率
	ResponseTime    int64   `json:"response_time"`     // 响应时间(ms)
	RequestCount    int     `json:"request_count"`     // 当前请求数
	LastUpdateTime  int64   `json:"last_update_time"`  // 最后更新时间戳
	IsPermanentDown bool    `json:"is_permanent_down"` // 是否长期不可用
}

type Blockchain struct {
	BatchID string  `json:"batch_id"`
	Chain   []Block `json:"chain"`
}

type ProcessingResponse struct {
	Success    bool   `json:"success"`
	BatchID    string `json:"batch_id"`
	BlockIndex int    `json:"block_index"`
	BlockHash  string `json:"block_hash"`
	PrevHash   string `json:"prev_hash"`
	Timestamp  int64  `json:"timestamp"`
}

type BatchInfo struct {
	BatchID      string    `json:"batch_id"`
	ProductType  string    `json:"product_type"` // 产品类型
	CreationDate time.Time `json:"creation_date"`
	CurrentLevel int       `json:"current_level"`
	CurrentHash  string    `json:"current_hash"`
	Complete     bool      `json:"complete"` // 是否已完成全流程
}

// boolToInt 将布尔值转换为整数（true=1, false=0）
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// 用户相关数据结构
type User struct {
	ID            int       `json:"id"`
	Username      string    `json:"username"`
	PasswordHash  string    `json:"-"` // 不返回密码哈希
	Email         string    `json:"email"`
	FullName      string    `json:"full_name"`
	Role          string    `json:"role"`           // 角色：admin, producer, processor1, processor2等
	AllowedLevels []int     `json:"allowed_levels"` // 允许处理的级别
	CreatedAt     time.Time `json:"created_at"`
	LastLogin     time.Time `json:"last_login"`
	IsActive      bool      `json:"is_active"`
}

type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type LoginResponse struct {
	Success bool   `json:"success"`
	Token   string `json:"token"`
	User    User   `json:"user"`
	Message string `json:"message"`
}

type RegisterRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Email    string `json:"email"`
	FullName string `json:"full_name"`
	Role     string `json:"role"`
}

// 错误响应结构
type ErrorResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
}

// 图片上传请求结构
type ImageUploadRequest struct {
	BatchID   string `json:"batch_id"`
	Level     int    `json:"level"`
	ImageHash string `json:"image_hash"` // 客户端计算的图片哈希
}

// 已移除检测结果上传请求结构体

// 已移除YOLO服务回调数据结构

// 已移除单个检测结果结构体

// ------------------------------- 全局变量 --------------------------------
var (
	blockchains map[string]Blockchain // 按批次ID存储的区块链
	batchInfos  map[string]BatchInfo  // 批次元数据
	users       map[string]User       // 按用户名存储的用户
	db          *sql.DB
	dataDir     string // 数据存储目录

	// P2P网络相关变量
	p2pServer           net.Listener      // P2P服务器监听器
	knownNodes          []NodeInfo        // 已知节点列表
	thisNodeID          string            // 当前节点ID
	p2pPort             = 3000            // P2P网络端口
	p2pMutex            sync.RWMutex      // 保护节点列表的互斥锁
	nodeCleanupInterval = 5 * time.Minute // 节点清理间隔

	// 区块链同步相关
	syncMutex sync.RWMutex // 保护区块链数据的互斥锁

	// 非对称加密相关
	privateKey   *rsa.PrivateKey                   // 当前节点的私钥
	publicKeyMap = make(map[string]*rsa.PublicKey) // 其他节点的公钥映射（节点ID -> 公钥）

	// 主节点配置
	mainNodes = []string{ // 直接配置主节点地址列表
		// 注意：这里应该使用P2P服务端口(3000)而不是HTTP服务端口(8080)
		"111.230.7.188:3000",  // 主节点1的IP和P2P端口
		"152.136.16.231:3000", // 主节点2的IP和P2P端口
	}
	isMainNode        bool                    // 当前节点是否为主节点
	currentMainNode   string                  // 当前首选主节点
	lastSyncTime      time.Time               // 上次同步时间
	loadBalancingInfo map[string]NodeLoadInfo // 各节点负载信息
	loadInfoMutex     sync.RWMutex            // 保护负载信息的互斥锁
)
var imagesDir = filepath.Join(dataDir, "images") // 图片存储目录
// 已移除detectionsDir变量（不再需要检测结果图片目录）
var mutex = &sync.Mutex{}
var productType = "wool"                                       // 产品类型(可根据需要修改)
var maxProcessLevel = 7                                        // 最大处理级别
var jwtSecret = []byte("your-secret-key-change-in-production") // JWT密钥，生产环境需修改
var jwtExpiryHours = 24                                        // JWT过期时间(小时)
// 允许的前端域名列表，包含YOLO检测服务
var allowedOrigins = []string{
	"http://localhost:5500",
	"http://127.0.0.1:5500",
	"http://localhost:3000",
	"http://127.0.0.1:3000",
}

// 批次ID正则表达式，用于提取UUID和时间戳
var batchIDRegex = regexp.MustCompile(`^batch_([0-9a-fA-F-]+)_(\d+)$`)

// JWT声明结构
type Claims struct {
	Username string `json:"username"`
	Role     string `json:"role"`
	Levels   []int  `json:"levels"`
	jwt.StandardClaims
}

// ------------------------------- 初始化 --------------------------------
func init() {
	// 根据操作系统设置数据目录
	if runtime.GOOS == "windows" {
		// Windows系统使用USERPROFILE环境变量
		userProfile := os.Getenv("USERPROFILE")
		if userProfile != "" {
			dataDir = filepath.Join(userProfile, ".product-chain")
		} else {
			// 如果USERPROFILE不存在，使用当前目录
			dataDir = "./.product-chain"
		}
	} else {
		// Linux/Mac系统使用HOME环境变量
		homeDir := os.Getenv("HOME")
		if homeDir != "" {
			dataDir = filepath.Join(homeDir, ".product-chain")
		} else {
			// 如果HOME不存在，使用当前目录
			dataDir = "./.product-chain"
		}
	}
	log.Printf("使用数据目录: %s", dataDir)

	// 创建数据目录和子目录
	if err := os.MkdirAll(filepath.Join(dataDir, "qrcodes"), 0755); err != nil {
		log.Fatalf("Failed to create data directory: %v", err)
	}

	// 创建图片存储目录
	if err := os.MkdirAll(imagesDir, 0755); err != nil {
		log.Fatalf("Failed to create images directory: %v", err)
	}

	// 检测结果图片存储目录已不再需要（检测功能已移除）

	// 设置日志输出
	setupLogging()

	// 初始化数据结构
	blockchains = make(map[string]Blockchain)
	batchInfos = make(map[string]BatchInfo)
	users = make(map[string]User)

	// 初始化数据库
	initDatabase()

	// 初始化区块链
	initBlockchain()

	// 确保管理员用户存在
	ensureAdminUser()
}

func setupLogging() {
	logFile, err := os.OpenFile(filepath.Join(dataDir, "server.log"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatal(err)
	}

	multiWriter := io.MultiWriter(os.Stdout, logFile)
	log.SetOutput(multiWriter)
	log.SetFlags(log.LstdFlags | log.Lshortfile)
}

func initDatabase() {
	var err error
	dbPath := filepath.Join(dataDir, "products.db")
	db, err = sql.Open("sqlite3", dbPath+"?_timeout=5000")
	if err != nil {
		log.Fatal(err)
	}

	// 配置连接池
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(25)
	db.SetConnMaxLifetime(5 * time.Minute)

	// 创建用户表
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS users (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		username TEXT UNIQUE NOT NULL,
		password_hash TEXT NOT NULL,
		email TEXT UNIQUE NOT NULL,
		full_name TEXT,
		role TEXT NOT NULL,
		allowed_levels TEXT NOT NULL, -- 用逗号分隔的级别列表
		created_at INTEGER NOT NULL,
		last_login INTEGER,
		is_active INTEGER DEFAULT 1
	)`)
	if err != nil {
		log.Fatal(err)
	}

	// 创建产品溯源表 - 增加检测结果哈希字段
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS product_chain (
		batch_id TEXT,
		block_index INTEGER,
		level INTEGER,
		timestamp INTEGER,
		processor_id TEXT,
		process_date TEXT,
		details TEXT,
		quality_check TEXT,
		location TEXT,
		note TEXT,
		image_hash TEXT,
		detection_hash TEXT, -- 检测结果图片哈希
		previous_hash TEXT,
		current_hash TEXT,
		PRIMARY KEY (batch_id, block_index)
	)`)
	if err != nil {
		log.Fatal(err)
	}

	// 创建批次信息表
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS batch_info (
		batch_id TEXT PRIMARY KEY,
		product_type TEXT,
		creation_date INTEGER,
		current_level INTEGER,
		current_hash TEXT,
		complete INTEGER
	)`)
	if err != nil {
		log.Fatal(err)
	}

	// 从数据库加载数据
	loadUsersFromDB()
	loadBlockchainsFromDB()
}

func loadUsersFromDB() { //（ai协助开发@deepseek@chatGPT）
	rows, err := db.Query("SELECT id, username, password_hash, email, full_name, role, allowed_levels, created_at, last_login, is_active FROM users")
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()

	for rows.Next() {
		var user User
		var allowedLevelsStr string
		var createdAt int64
		var lastLogin sql.NullInt64 // 使用sql.NullInt64处理可能的NULL值

		if err := rows.Scan(&user.ID, &user.Username, &user.PasswordHash, &user.Email,
			&user.FullName, &user.Role, &allowedLevelsStr, &createdAt, &lastLogin, &user.IsActive); err != nil {
			log.Fatal(err)
		}

		// 解析允许的处理级别
		levels := []int{}
		if allowedLevelsStr != "" {
			parts := strings.Split(allowedLevelsStr, ",")
			for _, part := range parts {
				var level int
				fmt.Sscanf(part, "%d", level)
				levels = append(levels, level)
			}
		}

		user.AllowedLevels = levels
		user.CreatedAt = time.Unix(createdAt, 0)
		// 检查last_login是否有值
		if lastLogin.Valid {
			user.LastLogin = time.Unix(lastLogin.Int64, 0)
		} else {
			user.LastLogin = time.Time{} // 使用零值时间
		}

		users[user.Username] = user
	}
}

func initBlockchain() {
	blockchainFile := filepath.Join(dataDir, "blockchains.json")

	if data, err := os.ReadFile(blockchainFile); err == nil {
		var chains []Blockchain
		if err := json.Unmarshal(data, &chains); err == nil {
			for _, chain := range chains {
				blockchains[chain.BatchID] = chain
				if len(chain.Chain) > 0 {
					lastBlock := chain.Chain[len(chain.Chain)-1]
					batchInfos[chain.BatchID] = BatchInfo{
						BatchID:      chain.BatchID,
						ProductType:  productType,
						CreationDate: time.Unix(lastBlock.Timestamp, 0),
						CurrentLevel: lastBlock.Level,
						CurrentHash:  lastBlock.CurrentHash,
						Complete:     lastBlock.Level >= maxProcessLevel,
					}
				}
			}
			return
		}
		log.Printf("Failed to load blockchains: %v", err)
	}

	// 如果没有区块链数据，创建一个系统创世区块
	systemGenesis := Blockchain{
		BatchID: "system",
		Chain: []Block{{
			Index:     0,
			Timestamp: time.Now().Unix(),
			Level:     -1, // 系统级
			ProductInfo: ProductInfo{
				BatchID:     "system",
				ProcessorID: "system",
				ProcessDate: time.Now().Format("2006-01-02"),
				Details:     "System genesis block",
			},
			PreviousHash: "",
			CurrentHash:  calculateBlockHash(Block{Index: 0, Timestamp: time.Now().Unix()}),
		}},
	}
	blockchains["system"] = systemGenesis
	batchInfos["system"] = BatchInfo{
		BatchID:      "system",
		ProductType:  "system",
		CreationDate: time.Now(),
		CurrentLevel: -1,
		CurrentHash:  systemGenesis.Chain[0].CurrentHash,
		Complete:     true,
	}
	saveBlockchains()
}

func loadBlockchainsFromDB() {
	// 加载批次信息
	rows, err := db.Query("SELECT batch_id, product_type, creation_date, current_level, current_hash, complete FROM batch_info")
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()

	for rows.Next() {
		var info BatchInfo
		var creationDate int64
		var complete int
		if err := rows.Scan(&info.BatchID, &info.ProductType, &creationDate, &info.CurrentLevel,
			&info.CurrentHash, &complete); err != nil {
			log.Fatal(err)
		}
		info.CreationDate = time.Unix(creationDate, 0)
		info.Complete = complete == 1
		batchInfos[info.BatchID] = info
	}

	// 加载每个批次的区块链
	for batchID := range batchInfos {
		var chain []Block
		rows, err := db.Query("SELECT block_index, level, timestamp, processor_id, process_date, details, quality_check, location, note, image_hash, detection_hash, previous_hash, current_hash FROM product_chain WHERE batch_id = ? ORDER BY block_index", batchID)
		if err != nil {
			log.Fatal(err)
		}
		defer rows.Close()

		for rows.Next() {
			var block Block
			if err := rows.Scan(&block.Index, &block.Level, &block.Timestamp,
				&block.ProductInfo.ProcessorID, &block.ProductInfo.ProcessDate,
				&block.ProductInfo.Details, &block.ProductInfo.QualityCheck,
				&block.ProductInfo.Location, &block.ProductInfo.Note,
				&block.ProductInfo.ImageHash, &block.ProductInfo.DetectionHash,
				&block.PreviousHash, &block.CurrentHash); err != nil {
				log.Fatal(err)
			}
			block.ProductInfo.BatchID = batchID
			chain = append(chain, block)
		}

		if len(chain) > 0 {
			blockchains[batchID] = Blockchain{
				BatchID: batchID,
				Chain:   chain,
			}
		}
	}
}

// 确保管理员用户存在
func ensureAdminUser() {
	mutex.Lock()
	defer mutex.Unlock()

	// 检查管理员用户是否已存在
	if _, exists := users["admin"]; exists {
		return
	}

	// 创建默认管理员用户，密码为admin123
	passwordHash := hashPassword("admin123")
	allowedLevels := "0,1,2,3,4,5,6,7" // 管理员可以处理所有级别

	_, err := db.Exec(`INSERT INTO users (username, password_hash, email, full_name, role, allowed_levels, created_at, is_active)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"admin", passwordHash, "admin@example.com", "系统管理员", "admin",
		allowedLevels, time.Now().Unix(), 1)

	if err != nil {
		log.Printf("创建管理员用户失败: %v", err)
		return
	}

	// 添加到内存映射
	levels := []int{0, 1, 2, 3, 4, 5, 6, 7}
	users["admin"] = User{
		Username:      "admin",
		PasswordHash:  passwordHash,
		Email:         "admin@example.com",
		FullName:      "系统管理员",
		Role:          "admin",
		AllowedLevels: levels,
		CreatedAt:     time.Now(),
		IsActive:      true,
	}

	log.Println("创建了默认管理员用户: admin (密码: admin123)")
}

// ------------------------------- 工具函数 --------------------------------
func hashPassword(password string) string {
	hash := sha256.Sum256([]byte(password))
	return hex.EncodeToString(hash[:])
}

// 计算文件哈希值
func calculateFileHash(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}

	return hex.EncodeToString(hash.Sum(nil)), nil
}

func generateToken(user User) (string, error) {
	// 设置过期时间
	expirationTime := time.Now().Add(time.Duration(jwtExpiryHours) * time.Hour)

	// 创建claims
	claims := &Claims{
		Username: user.Username,
		Role:     user.Role,
		Levels:   user.AllowedLevels,
		StandardClaims: jwt.StandardClaims{
			ExpiresAt: expirationTime.Unix(),
			IssuedAt:  time.Now().Unix(),
			Issuer:    "product-chain-server",
		},
	}

	// 创建token
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)

	// 签名token
	tokenString, err := token.SignedString(jwtSecret)
	if err != nil {
		return "", err
	}

	return tokenString, nil
}

// 检查用户是否有权限处理特定级别
func hasLevelPermission(user User, level int) bool {
	// 管理员拥有所有权限
	if user.Role == "admin" {
		return true
	}

	// 检查用户是否被允许处理该级别
	for _, allowedLevel := range user.AllowedLevels {
		if allowedLevel == level {
			return true
		}
	}
	return false
}

// 检查来源是否在允许的列表中
func isOriginAllowed(origin string) bool {
	for _, allowed := range allowedOrigins {
		if origin == allowed {
			return true
		}
	}
	return false
}

// JWT认证中间件
func authenticateMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			sendErrorResponse(w, "缺少认证令牌", http.StatusUnauthorized, "MISSING_TOKEN")
			return
		}

		// 格式应为 "Bearer <token>"
		parts := strings.Split(authHeader, " ")
		if len(parts) != 2 || parts[0] != "Bearer" {
			sendErrorResponse(w, "无效的认证格式", http.StatusUnauthorized, "INVALID_FORMAT")
			return
		}

		tokenString := parts[1]

		// 解析token
		claims := &Claims{}
		token, err := jwt.ParseWithClaims(tokenString, claims, func(token *jwt.Token) (interface{}, error) {
			return jwtSecret, nil
		})

		if err != nil || !token.Valid {
			sendErrorResponse(w, "无效的认证令牌", http.StatusUnauthorized, "INVALID_TOKEN")
			return
		}

		// 检查用户是否存在
		user, exists := users[claims.Username]
		if !exists || !user.IsActive {
			sendErrorResponse(w, "用户不存在或已被禁用", http.StatusUnauthorized, "USER_DISABLED")
			return
		}

		// 将用户信息存入上下文
		ctx := context.WithValue(r.Context(), "user", user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// 级别权限检查中间件
func levelPermissionMiddleware(level int) mux.MiddlewareFunc {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 从上下文获取用户信息
			userVal := r.Context().Value("user")
			if userVal == nil {
				sendErrorResponse(w, "未授权访问", http.StatusUnauthorized, "UNAUTHORIZED")
				return
			}

			user, ok := userVal.(User)
			if !ok {
				sendErrorResponse(w, "无效的用户信息", http.StatusInternalServerError, "INVALID_USER_DATA")
				return
			}

			// 检查权限
			if !hasLevelPermission(user, level) {
				sendErrorResponse(w, "没有权限处理此阶段", http.StatusForbidden, "PERMISSION_DENIED")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// 发送标准化错误响应
func sendErrorResponse(w http.ResponseWriter, message string, statusCode int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(ErrorResponse{
		Success: false,
		Message: message,
		Code:    code,
	})
}

// ------------------------------- 区块链逻辑 --------------------------------
func calculateBlockHash(block Block) string {
	data := fmt.Sprintf("%s%d%d%d%s%s%s%s%s%s%s%s%s",
		block.ProductInfo.BatchID,
		block.Index, block.Timestamp, block.Level,
		block.ProductInfo.ProcessorID, block.ProductInfo.ProcessDate,
		block.ProductInfo.Details, block.ProductInfo.QualityCheck,
		block.ProductInfo.Location, block.ProductInfo.Note,
		block.ProductInfo.ImageHash, block.ProductInfo.DetectionHash,
		block.PreviousHash)
	h := sha256.New()
	h.Write([]byte(data))
	return hex.EncodeToString(h.Sum(nil))
}

func createNewBatch(batchID string, creator User) error {
	mutex.Lock()
	defer mutex.Unlock()

	if _, exists := blockchains[batchID]; exists {
		return fmt.Errorf("批次ID已存在")
	}

	// 检查用户是否有权限创建批次(级别0)
	if !hasLevelPermission(creator, 0) {
		return fmt.Errorf("没有权限创建新批次")
	}

	// 创建批次创世区块
	genesisBlock := Block{
		Index:     0,
		Timestamp: time.Now().Unix(),
		Level:     0, // 批次创世区块
		ProductInfo: ProductInfo{
			BatchID:     batchID,
			ProcessorID: creator.Username,
			ProcessDate: time.Now().Format("2006-01-02"),
			Details:     fmt.Sprintf("批次 %s 初始注册，创建者: %s", batchID, creator.Username),
		},
		PreviousHash: "",
	}
	genesisBlock.CurrentHash = calculateBlockHash(genesisBlock)

	// 创建新区块链
	newChain := Blockchain{
		BatchID: batchID,
		Chain:   []Block{genesisBlock},
	}
	blockchains[batchID] = newChain

	// 保存批次信息
	batchInfo := BatchInfo{
		BatchID:      batchID,
		ProductType:  productType,
		CreationDate: time.Now(),
		CurrentLevel: 0,
		CurrentHash:  genesisBlock.CurrentHash,
		Complete:     false,
	}
	batchInfos[batchID] = batchInfo

	// 保存到数据库
	_, err := db.Exec(`INSERT INTO product_chain VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		batchID, genesisBlock.Index, genesisBlock.Level, genesisBlock.Timestamp,
		genesisBlock.ProductInfo.ProcessorID, genesisBlock.ProductInfo.ProcessDate,
		genesisBlock.ProductInfo.Details, genesisBlock.ProductInfo.QualityCheck,
		genesisBlock.ProductInfo.Location, genesisBlock.ProductInfo.Note,
		genesisBlock.ProductInfo.ImageHash, genesisBlock.ProductInfo.DetectionHash,
		genesisBlock.PreviousHash, genesisBlock.CurrentHash)
	if err != nil {
		return err
	}

	_, err = db.Exec(`INSERT INTO batch_info VALUES(?,?,?,?,?,?)`,
		batchID, productType, batchInfo.CreationDate.Unix(),
		batchInfo.CurrentLevel, batchInfo.CurrentHash, 0)
	if err != nil {
		return err
	}

	saveBlockchains()
	log.Printf("用户 %s 创建了新批次: %s", creator.Username, batchID)

	// 如果是主节点，创建新批次后向其他主节点发送同步请求
	if isMainNode {
		go syncBlockchainWithMainNodes()
	}

	return nil
}

func addBlockToChain(batchID string, level int, info ProductInfo, user User) (Block, error) {
	mutex.Lock()
	defer mutex.Unlock()

	// 检查用户是否有权限处理此级别
	if !hasLevelPermission(user, level) {
		return Block{}, fmt.Errorf("没有权限处理此阶段")
	}

	chain, exists := blockchains[batchID]
	if !exists {
		return Block{}, fmt.Errorf("批次不存在")
	}

	lastBlock := chain.Chain[len(chain.Chain)-1]

	// 验证级别连续性
	if level != lastBlock.Level+1 {
		return Block{}, fmt.Errorf("级别顺序无效: 预期 %d, 实际 %d", lastBlock.Level+1, level)
	}

	newBlock := Block{
		Index:        lastBlock.Index + 1,
		Timestamp:    time.Now().Unix(),
		Level:        level,
		ProductInfo:  info,
		PreviousHash: lastBlock.CurrentHash,
	}
	newBlock.CurrentHash = calculateBlockHash(newBlock)

	// 更新区块链
	chain.Chain = append(chain.Chain, newBlock)
	blockchains[batchID] = chain

	// 更新批次信息
	batchInfo := batchInfos[batchID]
	batchInfo.CurrentLevel = level
	batchInfo.CurrentHash = newBlock.CurrentHash
	batchInfo.Complete = level >= maxProcessLevel
	batchInfos[batchID] = batchInfo

	// 保存到数据库
	_, err := db.Exec(`INSERT INTO product_chain VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		batchID, newBlock.Index, newBlock.Level, newBlock.Timestamp,
		newBlock.ProductInfo.ProcessorID, newBlock.ProductInfo.ProcessDate,
		newBlock.ProductInfo.Details, newBlock.ProductInfo.QualityCheck,
		newBlock.ProductInfo.Location, newBlock.ProductInfo.Note,
		newBlock.ProductInfo.ImageHash, newBlock.ProductInfo.DetectionHash,
		newBlock.PreviousHash, newBlock.CurrentHash)
	if err != nil {
		return Block{}, err
	}

	_, err = db.Exec(`UPDATE batch_info SET current_level = ?, current_hash = ?, complete = ? WHERE batch_id = ?`,
		batchInfo.CurrentLevel, batchInfo.CurrentHash, boolToInt(batchInfo.Complete), batchID)
	if err != nil {
		return Block{}, err
	}

	saveBlockchains()
	log.Printf("用户 %s 向批次 %s 添加了新区块 #%d (级别 %d)，哈希: %s",
		user.Username, batchID, newBlock.Index, newBlock.Level, newBlock.CurrentHash)

	// 如果是主节点，添加新处理信息后向其他主节点发送同步请求
	if isMainNode {
		go syncBlockchainWithMainNodes()
	}

	return newBlock, nil
}

func saveBlockchains() {
	var chains []Blockchain
	for _, chain := range blockchains {
		chains = append(chains, chain)
	}

	data, err := json.MarshalIndent(chains, "", "  ")
	if err != nil {
		log.Printf("Failed to marshal blockchains: %v", err)
		return
	}

	blockchainFile := filepath.Join(dataDir, "blockchains.json")
	if err := os.WriteFile(blockchainFile, data, 0644); err != nil {
		log.Printf("Failed to save blockchains: %v", err)
	}
}

// ------------------------------- 用户管理函数 --------------------------------
func createUser(username, password, email, fullName, role string) (User, error) {
	mutex.Lock()
	defer mutex.Unlock()

	// 检查用户是否已存在
	if _, exists := users[username]; exists {
		return User{}, fmt.Errorf("用户名已存在")
	}

	// 为不同角色设置默认允许的处理级别
	allowedLevels := []int{}
	switch role {
	case "admin":
		allowedLevels = []int{0, 1, 2, 3, 4, 5, 6, 7} // 管理员拥有所有权限
	case "producer":
		allowedLevels = []int{0, 1} // 生产者：负责批次注册和原材料登记（级别0,1）
	case "processor1":
		allowedLevels = []int{2, 3} // 一级加工者：负责初级加工和质量检测（级别2,3）
	case "processor2":
		allowedLevels = []int{4, 5} // 二级加工者：负责深度加工处理（级别4,5）
	case "distributor":
		allowedLevels = []int{6} // 分销商：负责物流运输环节（级别6）
	case "retailer":
		allowedLevels = []int{7} // 零售商：负责零售终端环节（级别7）
	default:
		return User{}, fmt.Errorf("无效的角色")
	}

	// 转换为字符串存储
	levelsStr := ""
	for i, level := range allowedLevels {
		if i > 0 {
			levelsStr += ","
		}
		levelsStr += fmt.Sprintf("%d", level)
	}

	// 哈希密码
	passwordHash := hashPassword(password)
	now := time.Now().Unix()

	// 保存到数据库
	result, err := db.Exec(`INSERT INTO users (username, password_hash, email, full_name, role, allowed_levels, created_at, is_active)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		username, passwordHash, email, fullName, role, levelsStr, now, 1)

	if err != nil {
		return User{}, err
	}

	// 获取新用户ID
	id, err := result.LastInsertId()
	if err != nil {
		return User{}, err
	}

	// 创建用户对象
	user := User{
		ID:            int(id),
		Username:      username,
		PasswordHash:  passwordHash,
		Email:         email,
		FullName:      fullName,
		Role:          role,
		AllowedLevels: allowedLevels,
		CreatedAt:     time.Unix(now, 0),
		IsActive:      true,
	}

	// 添加到内存映射
	users[username] = user

	log.Printf("创建了新用户: %s (角色: %s)", username, role)
	return user, nil
}

func authenticateUser(username, password string) (User, error) {
	mutex.Lock()
	defer mutex.Unlock()

	// 查找用户
	user, exists := users[username]
	if !exists {
		return User{}, fmt.Errorf("用户名或密码错误")
	}

	// 检查用户是否激活
	if !user.IsActive {
		return User{}, fmt.Errorf("用户已被禁用")
	}

	// 验证密码
	if hashPassword(password) != user.PasswordHash {
		return User{}, fmt.Errorf("用户名或密码错误")
	}

	// 更新最后登录时间
	now := time.Now().Unix()
	_, err := db.Exec("UPDATE users SET last_login = ? WHERE username = ?", now, username)
	if err != nil {
		log.Printf("更新最后登录时间失败: %v", err)
	} else {
		user.LastLogin = time.Unix(now, 0)
		users[username] = user // 更新内存中的用户信息
	}

	return user, nil
}

// ------------------------------- HTTP Handlers --------------------------------
func enableCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")

		// 检查来源是否在允许的列表中
		if origin != "" && isOriginAllowed(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
		}

		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Max-Age", "86400") // 24小时缓存预检请求

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func validateProductInfo(info ProductInfo) error {
	if info.BatchID == "" {
		return fmt.Errorf("batch_id is required")
	}
	if info.ProcessorID == "" {
		return fmt.Errorf("processor_id is required")
	}
	if _, err := time.Parse("2006-01-02", info.ProcessDate); err != nil {
		return fmt.Errorf("invalid process_date format, expected YYYY-MM-DD")
	}
	if info.Details == "" {
		return fmt.Errorf("details is required")
	}
	return nil
}

// 用户注册处理
func registerUserHandler(w http.ResponseWriter, r *http.Request) {
	var req RegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		log.Printf("注册请求JSON解析失败: %v", err)
		sendErrorResponse(w, fmt.Sprintf("JSON解析失败: %v", err), http.StatusBadRequest, "INVALID_JSON")
		return
	}

	// 验证输入
	if req.Username == "" || req.Password == "" {
		sendErrorResponse(w, "用户名和密码为必填项", http.StatusBadRequest, "MISSING_FIELDS")
		return
	}

	// 验证用户名长度 (4-20字符)
	if len(req.Username) < 4 || len(req.Username) > 20 {
		sendErrorResponse(w, "用户名长度应在4-20个字符之间", http.StatusBadRequest, "INVALID_USERNAME_LENGTH")
		return
	}

	// 验证密码强度 (至少8个字符)
	if len(req.Password) < 8 {
		sendErrorResponse(w, "密码长度应至少8个字符", http.StatusBadRequest, "INVALID_PASSWORD_LENGTH")
		return
	}

	// 验证邮箱格式
	if req.Email != "" && !isValidEmail(req.Email) {
		sendErrorResponse(w, "无效的邮箱格式", http.StatusBadRequest, "INVALID_EMAIL")
		return
	}

	// 验证角色是否有效
	validRoles := map[string]bool{
		"admin":       true,
		"producer":    true,
		"processor1":  true,
		"processor2":  true,
		"distributor": true,
		"retailer":    true,
	}
	if !validRoles[req.Role] {
		sendErrorResponse(w, "无效的角色类型", http.StatusBadRequest, "INVALID_ROLE")
		return
	}

	// 创建用户
	user, err := createUser(req.Username, req.Password, req.Email, req.FullName, req.Role)
	if err != nil {
		log.Printf("用户注册失败: %v, 用户名: %s", err, req.Username)
		sendErrorResponse(w, err.Error(), http.StatusBadRequest, "REGISTRATION_FAILED")
		return
	}

	log.Printf("用户注册成功: %s, 角色: %s", req.Username, req.Role)

	// 返回结果（不包含敏感信息）
	response := struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
		User    struct {
			Username string `json:"username"`
			Email    string `json:"email"`
			FullName string `json:"full_name"`
			Role     string `json:"role"`
		} `json:"user"`
	}{
		Success: true,
		Message: "用户注册成功",
	}

	response.User.Username = user.Username
	response.User.Email = user.Email
	response.User.FullName = user.FullName
	response.User.Role = user.Role

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// 简单的邮箱格式验证
func isValidEmail(email string) bool {
	if !strings.Contains(email, "@") {
		return false
	}
	parts := strings.Split(email, "@")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return false
	}
	return strings.Contains(parts[1], ".")
}

// 用户登录处理
func loginHandler(w http.ResponseWriter, r *http.Request) {
	var req LoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendErrorResponse(w, fmt.Sprintf("JSON解析失败: %v", err), http.StatusBadRequest, "INVALID_JSON")
		return
	}

	// 验证输入
	if req.Username == "" || req.Password == "" {
		sendErrorResponse(w, "用户名和密码为必填项", http.StatusBadRequest, "MISSING_FIELDS")
		return
	}

	// 验证用户
	user, err := authenticateUser(req.Username, req.Password)
	if err != nil {
		log.Printf("登录失败: %v, 用户名: %s", err, req.Username)
		sendErrorResponse(w, err.Error(), http.StatusUnauthorized, "LOGIN_FAILED")
		return
	}

	// 生成JWT令牌
	token, err := generateToken(user)
	if err != nil {
		http.Error(w, "生成令牌失败", http.StatusInternalServerError)
		log.Printf("生成JWT令牌失败: %v", err)
		return
	}

	// 返回结果
	response := LoginResponse{
		Success: true,
		Token:   token,
		Message: "登录成功",
		User:    user,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// 获取当前用户信息
func getCurrentUserHandler(w http.ResponseWriter, r *http.Request) {
	// 从上下文获取用户信息
	userVal := r.Context().Value("user")
	if userVal == nil {
		sendErrorResponse(w, "未授权访问", http.StatusUnauthorized, "UNAUTHORIZED")
		return
	}

	user, ok := userVal.(User)
	if !ok {
		sendErrorResponse(w, "无效的用户信息", http.StatusInternalServerError, "INVALID_USER_DATA")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(user)
}

// 验证令牌有效性端点
func validateTokenHandler(w http.ResponseWriter, r *http.Request) {
	// 从上下文获取用户信息
	userVal := r.Context().Value("user")
	if userVal == nil {
		sendErrorResponse(w, "令牌无效或已过期", http.StatusUnauthorized, "INVALID_TOKEN")
		return
	}

	user, ok := userVal.(User)
	if !ok {
		sendErrorResponse(w, "无效的用户信息", http.StatusInternalServerError, "INVALID_USER_DATA")
		return
	}

	// 返回成功响应表示令牌有效
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"message":  "令牌验证成功",
		"username": user.Username,
		"role":     user.Role,
		"valid":    true,
	})
}

// 注册批次处理
func registerBatchHandler(w http.ResponseWriter, r *http.Request) {
	// 从上下文获取用户信息
	userVal := r.Context().Value("user")
	if userVal == nil {
		sendErrorResponse(w, "未授权访问", http.StatusUnauthorized, "UNAUTHORIZED")
		return
	}

	user, ok := userVal.(User)
	if !ok {
		sendErrorResponse(w, "无效的用户信息", http.StatusInternalServerError, "INVALID_USER_DATA")
		return
	}

	var request struct {
		BatchID string `json:"batch_id"`
	}

	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		sendErrorResponse(w, fmt.Sprintf("JSON解析失败: %v", err), http.StatusBadRequest, "INVALID_JSON")
		return
	}

	if request.BatchID == "" {
		sendErrorResponse(w, "batch_id is required", http.StatusBadRequest, "MISSING_BATCH_ID")
		return
	}

	if err := createNewBatch(request.BatchID, user); err != nil {
		sendErrorResponse(w, err.Error(), http.StatusBadRequest, "BATCH_CREATION_FAILED")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"batch_id": request.BatchID,
		"message":  "批次注册成功",
	})
}

// 图片上传处理
func uploadImageHandler(w http.ResponseWriter, r *http.Request) {
	// 从上下文获取用户信息
	userVal := r.Context().Value("user")
	if userVal == nil {
		sendErrorResponse(w, "未授权访问", http.StatusUnauthorized, "UNAUTHORIZED")
		return
	}

	user, ok := userVal.(User)
	if !ok {
		sendErrorResponse(w, "无效的用户信息", http.StatusInternalServerError, "INVALID_USER_DATA")
		return
	}

	// 解析多部分表单
	r.ParseMultipartForm(10 << 20) // 10 MB

	// 获取文件句柄
	file, handler, err := r.FormFile("image")
	if err != nil {
		sendErrorResponse(w, "获取图片失败", http.StatusBadRequest, "IMAGE_UPLOAD_FAILED")
		return
	}
	defer file.Close()

	// 创建临时文件保存上传的图片
	tempFile, err := os.CreateTemp(imagesDir, "upload-*.png")
	if err != nil {
		sendErrorResponse(w, "保存图片失败", http.StatusInternalServerError, "IMAGE_SAVE_FAILED")
		return
	}
	defer tempFile.Close()

	// 将上传的图片内容复制到临时文件
	_, err = io.Copy(tempFile, file)
	if err != nil {
		sendErrorResponse(w, "处理图片失败", http.StatusInternalServerError, "IMAGE_PROCESS_FAILED")
		return
	}

	// 计算图片哈希值
	hash, err := calculateFileHash(tempFile.Name())
	if err != nil {
		sendErrorResponse(w, "计算图片哈希失败", http.StatusInternalServerError, "HASH_CALC_FAILED")
		return
	}

	// 重命名文件为哈希值
	newFileName := filepath.Join(imagesDir, hash+filepath.Ext(handler.Filename))
	if err := os.Rename(tempFile.Name(), newFileName); err != nil {
		sendErrorResponse(w, "重命名图片失败", http.StatusInternalServerError, "IMAGE_RENAME_FAILED")
		return
	}

	log.Printf("用户 %s 上传了图片，哈希值: %s", user.Username, hash)

	// 返回图片哈希值
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":    true,
		"image_hash": hash,
		"message":    "图片上传成功",
	})
}

// 检测结果图片上传处理（支持YOLO服务生成的图片）
// 注意：该功能的具体实现移至第3092行，此处保留注释

// 节点自动全同步协程
func startNodeAutoSync() {
	// 仅在启动时同步一次，不再定时同步
	// 数据更新时会通过其他机制触发同步
	syncBlockchainWithMainNodes()
}

// 与主节点同步区块链数据
func syncBlockchainWithMainNodes() {
	// 记录同步开始时间
	startTime := time.Now()

	// 如果是主节点，则同步到其他主节点
	if isMainNode {
		p2pMutex.RLock()
		for _, node := range knownNodes {
			if node.IsMainNode && node.ID != thisNodeID {
				// 对其他主节点执行全量同步
				syncFullChainToNode(node.Address)
			}
		}
		p2pMutex.RUnlock()
	} else {
		// 如果是从节点，从首选主节点同步
		if currentMainNode != "" {
			syncFullChainFromNode(currentMainNode)
		}
	}

	// 更新最后同步时间
	lastSyncTime = startTime
	log.Printf("区块链同步完成，耗时: %v", time.Since(startTime))
}

// 向指定节点同步完整区块链
func syncFullChainToNode(nodeAddress string) {
	// 确保节点地址包含有效的端口号
	if !strings.Contains(nodeAddress, ":") || strings.HasSuffix(nodeAddress, ":0") || strings.HasSuffix(nodeAddress, ":") {
		// 如果地址没有端口号或端口号为0，则修正为默认P2P端口
		if strings.Contains(nodeAddress, ":") {
			// 移除无效的端口号
			nodeAddress = strings.Split(nodeAddress, ":")[0]
		}
		nodeAddress = fmt.Sprintf("%s:%d", nodeAddress, p2pPort)
		log.Printf("修正节点地址为: %s", nodeAddress)
	}

	// 添加重试机制，最多尝试5次连接（增强版）
	var conn net.Conn
	var err error
	maxRetries := 5
	retryDelay := 2 * time.Second // 增加初始延迟时间
	var syncStartTime = time.Now()
	log.Printf("开始向节点 %s 同步区块链数据", nodeAddress)

	for attempt := 1; attempt <= maxRetries; attempt++ {
		conn, err = net.DialTimeout("tcp", nodeAddress, 15*time.Second) // 增加连接超时时间
		if err == nil {
			log.Printf("第 %d 次尝试连接节点 %s 成功", attempt, nodeAddress)
			break
		}

		log.Printf("第 %d 次尝试连接节点 %s 失败: %v", attempt, nodeAddress, err)
		if attempt < maxRetries {
			time.Sleep(retryDelay)
			retryDelay *= 2 // 指数退避
			log.Printf("下一次重试延迟: %v", retryDelay)
		}
	}

	if err != nil {
		log.Printf("连接节点 %s 失败，已重试 %d 次: %v", nodeAddress, maxRetries, err)
		// 标记节点为不可用但保留重试机会
		markNodeAsUnavailable(nodeAddress, false) // false表示临时不可用
		return
	}
	defer conn.Close()

	// 设置写入超时，增加超时时间
	conn.SetWriteDeadline(time.Now().Add(60 * time.Second))

	// 获取所有批次ID
	syncMutex.RLock()
	batchIDs := make([]string, 0, len(blockchains))
	for batchID := range blockchains {
		batchIDs = append(batchIDs, batchID)
	}
	syncMutex.RUnlock()

	// 统计失败的批次数量
	failedBatchCount := 0
	successfulBatchCount := 0
	var failedBatches []string

	// 对每个批次直接发送全量区块链数据（而不是发送请求）
	for _, batchID := range batchIDs {
		syncMutex.RLock()
		chain, exists := blockchains[batchID]
		syncMutex.RUnlock()

		if exists && len(chain.Chain) > 0 {
			// 准备完整的区块数据
			blocksToSend := []map[string]interface{}{}
			for _, block := range chain.Chain {
				fullBlock := map[string]interface{}{
					"index":         block.Index,
					"timestamp":     block.Timestamp,
					"level":         block.Level,
					"previous_hash": block.PreviousHash,
					"current_hash":  block.CurrentHash,
					"merkle_root":   block.MerkleRoot,
					// 包含完整的产品信息
					"product_info": map[string]interface{}{
						"batch_id":       block.ProductInfo.BatchID,
						"processor_id":   block.ProductInfo.ProcessorID,
						"process_date":   block.ProductInfo.ProcessDate,
						"details":        block.ProductInfo.Details,
						"note":           block.ProductInfo.Note,
						"image_hash":     block.ProductInfo.ImageHash,
						"detection_hash": block.ProductInfo.DetectionHash,
						"location":       block.ProductInfo.Location,
						"quality_check":  block.ProductInfo.QualityCheck,
					},
				}
				blocksToSend = append(blocksToSend, fullBlock)
			}

			// 创建全量数据发送消息
			message := map[string]interface{}{
				"type":           "blockchain_full_batch_data",
				"success":        true,
				"batch_id":       batchID,
				"blocks":         blocksToSend,
				"total_blocks":   len(chain.Chain),
				"sender_node_id": thisNodeID,
				"is_main_node":   isMainNode,
			}

			// 发送数据
			e := json.NewEncoder(conn)
			if err := e.Encode(message); err != nil {
				log.Printf("向节点 %s 发送全量区块链数据失败 (批次 %s): %v", nodeAddress, batchID, err)
				failedBatchCount++
				failedBatches = append(failedBatches, batchID)

				// 如果是broken pipe错误，重新连接
				if strings.Contains(err.Error(), "broken pipe") {
					log.Printf("检测到broken pipe错误，重新连接到节点 %s", nodeAddress)
					conn.Close()

					// 重新连接，使用更长的超时时间
					conn, err = net.DialTimeout("tcp", nodeAddress, 15*time.Second)
					if err != nil {
						log.Printf("重新连接节点 %s 失败: %v", nodeAddress, err)
						// 标记节点为临时不可用
						markNodeAsUnavailable(nodeAddress, false)
						return
					}
					conn.SetWriteDeadline(time.Now().Add(60 * time.Second))

					// 重新尝试发送数据
					e = json.NewEncoder(conn)
					if err := e.Encode(message); err != nil {
						log.Printf("重新发送全量区块链数据失败 (批次 %s): %v", batchID, err)
						continue
					} else {
						// 重连后发送成功
						successfulBatchCount++
					}
				} else {
					// 对于其他错误，继续处理下一个批次
					continue
				}
			} else {
				// 数据发送成功
				successfulBatchCount++
			}

			// 短暂延迟避免请求过于频繁
			time.Sleep(100 * time.Millisecond)

			// 清理blocksToSend占用的内存
			blocksToSend = nil
		}
	}

	// 发送完所有请求后，设置读取超时，准备接收响应
	conn.SetReadDeadline(time.Now().Add(120 * time.Second))
	decoder := json.NewDecoder(conn)

	// 尝试接收响应并处理
	hasReceivedResponse := false
	for {
		var msg map[string]interface{}
		if err := decoder.Decode(&msg); err != nil {
			// 如果是EOF或者超时错误，结束接收
			if err == io.EOF || strings.Contains(err.Error(), "i/o timeout") {
				break
			}
			// 记录其他错误但不中断流程
			log.Printf("接收同步响应时发生错误: %v", err)
			continue
		}

		// 获取消息类型
		msgType, typeOk := msg["type"].(string)
		if typeOk {
			hasReceivedResponse = true
			// 根据消息类型处理响应
			switch msgType {
			case "blockchain_full_batch_data":
				// 对于成功的全量同步数据，直接调用handleBlockchainSyncResponse处理
				handleBlockchainSyncResponse(conn, msg)
			case "blockchain_full_sync_response":
				// 对于失败的响应，也调用handleBlockchainSyncResponse处理
				handleBlockchainSyncResponse(conn, msg)
			default:
				log.Printf("收到未知类型的同步响应: %s", msgType)
			}
		}
	}

	// 如果没有收到任何响应，记录警告
	if !hasReceivedResponse {
		log.Printf("警告: 向节点 %s 发送同步请求后未收到任何响应", nodeAddress)
	}

	// 详细的同步状态报告
	totalBatches := len(batchIDs)
	syncDuration := time.Since(syncStartTime)
	log.Printf("向节点 %s 同步完成 - 总计: %d 批次, 成功: %d 批次, 失败: %d 批次, 耗时: %v",
		nodeAddress, totalBatches, successfulBatchCount, failedBatchCount, syncDuration)

	// 如果有失败的批次，记录具体是哪些批次
	if failedBatchCount > 0 {
		log.Printf("失败的批次ID列表: %v", failedBatches)
	}

	// 增强的节点可用性判断
	successRate := float64(successfulBatchCount) / float64(totalBatches)
	if totalBatches > 0 && successRate < 0.3 {
		// 如果成功率低于30%，标记为长期不可用
		log.Printf("节点 %s 同步成功率过低 (%.1f%%)，标记为长期不可用", nodeAddress, successRate*100)
		markNodeAsUnavailable(nodeAddress, true)
	} else if failedBatchCount > 0 {
		// 如果有部分失败但成功率不低于30%，标记为临时不可用
		log.Printf("节点 %s 存在部分同步失败，标记为临时不可用，建议后续重新同步失败批次", nodeAddress)
		markNodeAsUnavailable(nodeAddress, false)
	}

	// 如果失败批次过多，尝试稍后重新同步失败的批次
	if failedBatchCount > 0 {
		go func() {
			// 等待一段时间后尝试重新同步失败的批次
			time.Sleep(2 * time.Minute)
			log.Printf("开始重新同步上次失败的批次到节点 %s", nodeAddress)
			syncFailedBatchesToNode(nodeAddress, failedBatches)
		}()
	}
}

// 标记节点为不可用
// isPermanent: true表示长期不可用，需要人工干预；false表示临时不可用，系统会尝试重新连接
func markNodeAsUnavailable(nodeAddress string, isPermanent bool) {
	loadInfoMutex.Lock()
	defer loadInfoMutex.Unlock()

	// 更新节点负载信息，设置一个非常差的评分
	loadBalancingInfo[nodeAddress] = NodeLoadInfo{
		CPUUsage:        100.0,  // 100% CPU使用率表示不可用
		MemoryUsage:     100.0,  // 100%内存使用率表示不可用
		ResponseTime:    999999, // 很高的响应时间表示不可用
		RequestCount:    999999, // 很高的请求数表示不可用
		LastUpdateTime:  time.Now().Unix(),
		IsPermanentDown: isPermanent, // 新增字段：是否长期不可用
	}

	if isPermanent {
		log.Printf("已标记节点 %s 为长期不可用，请检查该节点状态", nodeAddress)
	} else {
		log.Printf("已标记节点 %s 为临时不可用，系统将尝试重新连接", nodeAddress)
	}

	// 如果是临时不可用，启动一个定期检查的协程
	if !isPermanent {
		go func() {
			checkInterval := 5 * time.Minute
			maxChecks := 5
			for i := 1; i <= maxChecks; i++ {
				time.Sleep(checkInterval)
				log.Printf("第 %d 次检查节点 %s 是否恢复可用", i, nodeAddress)

				// 尝试简单连接测试
				conn, err := net.DialTimeout("tcp", nodeAddress, 5*time.Second)
				if err == nil {
					conn.Close()
					log.Printf("节点 %s 已恢复可用，清除不可用标记", nodeAddress)
					// 清除不可用标记（通过设置正常的负载信息）
					loadInfoMutex.Lock()
					loadBalancingInfo[nodeAddress] = NodeLoadInfo{
						CPUUsage:        0.0,
						MemoryUsage:     0.0,
						ResponseTime:    0,
						RequestCount:    0,
						LastUpdateTime:  time.Now().Unix(),
						IsPermanentDown: false,
					}
					loadInfoMutex.Unlock()
					break
				}
				log.Printf("节点 %s 仍不可用，%v 后再次检查", nodeAddress, checkInterval)
			}
		}()
	}
}

// 重新同步失败的批次数据到指定节点
func syncFailedBatchesToNode(nodeAddress string, failedBatches []string) {
	if len(failedBatches) == 0 {
		return
	}

	log.Printf("开始重新同步 %d 个失败的批次到节点 %s", len(failedBatches), nodeAddress)

	// 尝试连接节点
	conn, err := net.DialTimeout("tcp", nodeAddress, 15*time.Second)
	if err != nil {
		log.Printf("重新连接节点 %s 失败，无法重新同步: %v", nodeAddress, err)
		return
	}
	defer conn.Close()

	conn.SetWriteDeadline(time.Now().Add(60 * time.Second))

	resyncSuccessCount := 0
	resyncFailCount := 0

	for _, batchID := range failedBatches {
		// 获取该批次的链数据
		syncMutex.RLock()
		chain, exists := blockchains[batchID]
		syncMutex.RUnlock()

		if exists && len(chain.Chain) > 0 {
			// 创建同步请求
			request := map[string]interface{}{
				"type":          "blockchain_full_sync_request",
				"batch_id":      batchID,
				"node_id":       thisNodeID,
				"is_main_node":  isMainNode,
				"retry_attempt": true,
			}

			// 发送请求
			e := json.NewEncoder(conn)
			if err := e.Encode(request); err != nil {
				log.Printf("重新同步批次 %s 失败: %v", batchID, err)
				resyncFailCount++
			} else {
				log.Printf("重新同步批次 %s 成功", batchID)
				resyncSuccessCount++
			}

			// 短暂延迟
			time.Sleep(100 * time.Millisecond)
		}
	}

	log.Printf("失败批次重同步完成 - 成功: %d, 失败: %d", resyncSuccessCount, resyncFailCount)

	// 如果重同步后大部分批次成功，更新节点状态
	if len(failedBatches) > 0 && float64(resyncSuccessCount)/float64(len(failedBatches)) > 0.7 {
		log.Printf("重同步成功率超过70%%，恢复节点 %s 的可用状态", nodeAddress)
		markNodeAsAvailable(nodeAddress)
	}
}

// 标记节点为可用
func markNodeAsAvailable(nodeAddress string) {
	loadInfoMutex.Lock()
	defer loadInfoMutex.Unlock()

	loadBalancingInfo[nodeAddress] = NodeLoadInfo{
		CPUUsage:        0.0,
		MemoryUsage:     0.0,
		ResponseTime:    0,
		RequestCount:    0,
		LastUpdateTime:  time.Now().Unix(),
		IsPermanentDown: false,
	}

	log.Printf("已恢复节点 %s 为可用状态", nodeAddress)
}

// 从指定节点同步完整区块链
func syncFullChainFromNode(nodeAddress string) {
	// 确保节点地址包含有效的端口号
	if !strings.Contains(nodeAddress, ":") || strings.HasSuffix(nodeAddress, ":0") || strings.HasSuffix(nodeAddress, ":") {
		// 如果地址没有端口号或端口号为0，则修正为默认P2P端口
		if strings.Contains(nodeAddress, ":") {
			// 移除无效的端口号
			nodeAddress = strings.Split(nodeAddress, ":")[0]
		}
		nodeAddress = fmt.Sprintf("%s:%d", nodeAddress, p2pPort)
		log.Printf("修正节点地址为: %s", nodeAddress)
	}

	// 添加重试机制，最多尝试3次连接
	var conn net.Conn
	var err error
	maxRetries := 3
	retryDelay := time.Second

	for attempt := 1; attempt <= maxRetries; attempt++ {
		conn, err = net.DialTimeout("tcp", nodeAddress, 10*time.Second)
		if err == nil {
			break
		}

		log.Printf("第 %d 次尝试连接主节点 %s 失败: %v", attempt, nodeAddress, err)
		if attempt < maxRetries {
			time.Sleep(retryDelay)
			retryDelay *= 2 // 指数退避
		}
	}

	if err != nil {
		log.Printf("连接主节点 %s 失败，已重试 %d 次: %v", nodeAddress, maxRetries, err)
		// 标记节点为不可用
		markNodeAsUnavailable(nodeAddress, false)
		return
	}
	defer conn.Close()

	// 设置读写超时
	conn.SetDeadline(time.Now().Add(60 * time.Second))

	// 请求获取区块链概览
	request := map[string]interface{}{
		"type":    "blockchain_overview_request",
		"node_id": thisNodeID,
	}

	e := json.NewEncoder(conn)
	if err := e.Encode(request); err != nil {
		log.Printf("向主节点 %s 发送概览请求失败: %v", nodeAddress, err)
		// 标记节点为不可用
		markNodeAsUnavailable(nodeAddress, false)
		return
	}

	// 处理响应
	d := json.NewDecoder(conn)
	for {
		// 更新读取超时
		conn.SetReadDeadline(time.Now().Add(30 * time.Second))

		var response map[string]interface{}
		if err := d.Decode(&response); err != nil {
			if err != io.EOF {
				log.Printf("解析主节点响应失败: %v，可能是连接了错误的端口(应该使用P2P端口3000而不是HTTP端口8080)", err)
				// 如果是超时或网络错误，标记节点为不可用
				if strings.Contains(err.Error(), "timeout") || strings.Contains(err.Error(), "connection") {
					markNodeAsUnavailable(nodeAddress, false)
				}
			}
			break
		}

		// 安全地提取响应类型
		var responseType string
		if responseTypeVal, ok := response["type"].(string); ok {
			responseType = responseTypeVal
		}

		switch responseType {
		case "blockchain_overview_response":
			// 获取到概览后，请求需要同步的批次
			batchIDs := extractBatchIDsFromOverview(response)
			for _, batchID := range batchIDs {
				requestFullBatchFromNode(conn, batchID)
			}
		case "blockchain_full_batch_data":
			// 处理接收到的完整批次数据
			processFullBatchData(response)
		}
	}
}

// 从概览响应中提取批次ID（ai协助开发@deepseek）
func extractBatchIDsFromOverview(response map[string]interface{}) []string {
	var batchIDs []string
	// 安全地提取概览数据
	if overviewVal, ok := response["blockchain_overview"].([]interface{}); ok {
		for _, item := range overviewVal {
			// 安全地提取批次项
			if batchItem, ok := item.(map[string]interface{}); ok {
				// 安全地提取批次ID
				if batchIDVal, ok := batchItem["batch_id"].(string); ok {
					if batchIDVal != "" {
						batchIDs = append(batchIDs, batchIDVal)
					}
				}
			}
		}
	}
	return batchIDs
}

// 向节点请求完整批次数据
func requestFullBatchFromNode(conn net.Conn, batchID string) {
	request := map[string]interface{}{
		"type":     "blockchain_full_batch_request",
		"batch_id": batchID,
	}
	e := json.NewEncoder(conn)
	if err := e.Encode(request); err != nil {
		log.Printf("发送节点注册请求失败: %v", err)
		return
	}
	// 短暂延迟
	time.Sleep(50 * time.Millisecond)
}

// 处理接收到的完整批次数据
func processFullBatchData(response map[string]interface{}) {
	// 安全地提取批次ID
	var batchID string
	if batchIDVal, ok := response["batch_id"].(string); ok {
		batchID = batchIDVal
	}

	// 安全地提取成功标志
	var success bool
	if successVal, ok := response["success"].(bool); ok {
		success = successVal
	}

	if !success || batchID == "" {
		return
	}

	// 安全地提取区块数据
	var blocksData []interface{}
	if blocksDataVal, ok := response["blocks"].([]interface{}); ok {
		blocksData = blocksDataVal
	}
	if len(blocksData) == 0 {
		return
	}

	var blocks []Block
	for _, blockData := range blocksData {
		blockMap, _ := blockData.(map[string]interface{})
		block := Block{
			Index:        0,
			Timestamp:    0,
			Level:        0,
			PreviousHash: "",
			CurrentHash:  "",
		}

		// 安全地提取区块索引
		if indexVal, ok := blockMap["index"]; ok {
			if indexFloat, ok := indexVal.(float64); ok {
				block.Index = int(indexFloat)
			}
		}

		// 安全地提取时间戳
		if timestampVal, ok := blockMap["timestamp"]; ok {
			if timestampFloat, ok := timestampVal.(float64); ok {
				block.Timestamp = int64(timestampFloat)
			}
		}

		// 安全地提取级别
		if levelVal, ok := blockMap["level"]; ok {
			if levelFloat, ok := levelVal.(float64); ok {
				block.Level = int(levelFloat)
			}
		}

		// 安全地提取哈希值
		if previousHash, ok := blockMap["previous_hash"].(string); ok {
			block.PreviousHash = previousHash
		}

		if currentHash, ok := blockMap["current_hash"].(string); ok {
			block.CurrentHash = currentHash
		}

		// 解析产品信息
		block.ProductInfo = ProductInfo{
			BatchID:       "",
			ProcessorID:   "",
			ProcessDate:   "",
			Details:       "",
			Note:          "",
			ImageHash:     "",
			DetectionHash: "",
			Location:      "",
			QualityCheck:  "",
		}

		// 安全地提取产品信息
		if productInfoMap, ok := blockMap["product_info"].(map[string]interface{}); ok {
			// 优先尝试提取batchID，如果不存在则尝试batch_id（兼容旧版本）
			if batchID, ok := productInfoMap["batchID"].(string); ok {
				block.ProductInfo.BatchID = batchID
			} else if batchID, ok := productInfoMap["batch_id"].(string); ok {
				block.ProductInfo.BatchID = batchID
			}
			if processorID, ok := productInfoMap["processor_id"].(string); ok {
				block.ProductInfo.ProcessorID = processorID
			}
			if processDate, ok := productInfoMap["process_date"].(string); ok {
				block.ProductInfo.ProcessDate = processDate
			}
			if details, ok := productInfoMap["details"].(string); ok {
				block.ProductInfo.Details = details
			}
			if note, ok := productInfoMap["note"].(string); ok {
				block.ProductInfo.Note = note
			}
			if imageHash, ok := productInfoMap["image_hash"].(string); ok {
				block.ProductInfo.ImageHash = imageHash
			}
			if detectionHash, ok := productInfoMap["detection_hash"].(string); ok {
				block.ProductInfo.DetectionHash = detectionHash
			}
			if location, ok := productInfoMap["location"].(string); ok {
				block.ProductInfo.Location = location
			}
			if qualityCheck, ok := productInfoMap["quality_check"].(string); ok {
				block.ProductInfo.QualityCheck = qualityCheck
			}
		}

		blocks = append(blocks, block)
	}

	// 更新本地区块链
	syncMutex.Lock()
	blockchains[batchID] = Blockchain{
		BatchID: batchID,
		Chain:   blocks,
	}

	// 更新批次信息
	if len(blocks) > 0 {
		lastBlock := blocks[len(blocks)-1]
		batchInfos[batchID] = BatchInfo{
			BatchID:      batchID,
			ProductType:  "", // 修复：使用空字符串代替未定义的productType变量
			CreationDate: time.Unix(lastBlock.Timestamp, 0),
			CurrentLevel: lastBlock.Level,
			CurrentHash:  lastBlock.CurrentHash,
			Complete:     lastBlock.Level >= maxProcessLevel,
		}
	}
	syncMutex.Unlock()

	// 保存到文件和数据库
	saveBlockchains()
	saveBatchInfoToDB(batchID)
}

// 将批次信息保存到数据库
func saveBatchInfoToDB(batchID string) {
	syncMutex.RLock()
	batchInfo, exists := batchInfos[batchID]
	syncMutex.RUnlock()

	if !exists {
		log.Printf("批次信息不存在: %s", batchID)
		return
	}

	// 检查数据库中是否已存在该批次信息
	var existsInDB bool
	err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM batch_info WHERE batch_id = ?)", batchID).Scan(&existsInDB)
	if err != nil {
		log.Printf("查询批次信息存在性失败: %v", err)
		return
	}

	// 插入或更新批次信息
	if existsInDB {
		_, err = db.Exec(`UPDATE batch_info SET product_type = ?, creation_date = ?, 
			current_level = ?, current_hash = ?, complete = ? WHERE batch_id = ?`,
			batchInfo.ProductType, batchInfo.CreationDate.Unix(),
			batchInfo.CurrentLevel, batchInfo.CurrentHash, boolToInt(batchInfo.Complete), batchID)
	} else {
		_, err = db.Exec(`INSERT INTO batch_info (batch_id, product_type, creation_date, 
			current_level, current_hash, complete) VALUES (?, ?, ?, ?, ?, ?)`,
			batchID, batchInfo.ProductType, batchInfo.CreationDate.Unix(),
			batchInfo.CurrentLevel, batchInfo.CurrentHash, boolToInt(batchInfo.Complete))
	}

	if err != nil {
		log.Printf("保存批次信息到数据库失败: %v", err)
	} else {
		log.Printf("成功保存批次信息到数据库: %s", batchID)
	}
}

// 处理区块的检测结果哈希
func updateBlockDetectionHash(batchID string, level int, detectionHash string, user User) error {
	mutex.Lock()
	defer mutex.Unlock()

	chain, exists := blockchains[batchID]
	if !exists {
		return fmt.Errorf("批次不存在")
	}

	// 查找对应级别的区块
	var targetBlock *Block
	for i := range chain.Chain {
		if chain.Chain[i].Level == level {
			targetBlock = &chain.Chain[i]
			break
		}
	}

	if targetBlock == nil {
		return fmt.Errorf("未找到级别为 %d 的区块", level)
	}

	// 更新检测哈希
	targetBlock.ProductInfo.DetectionHash = detectionHash

	// 重新计算哈希
	targetBlock.CurrentHash = calculateBlockHash(*targetBlock)

	// 如果不是最后一个区块，需要更新后续区块的哈希
	for i := targetBlock.Index + 1; i < len(chain.Chain); i++ {
		prevHash := chain.Chain[i-1].CurrentHash
		chain.Chain[i].PreviousHash = prevHash
		chain.Chain[i].CurrentHash = calculateBlockHash(chain.Chain[i])
	}

	// 更新区块链
	blockchains[batchID] = chain

	// 更新批次当前哈希
	if len(chain.Chain) > 0 {
		lastBlock := chain.Chain[len(chain.Chain)-1]
		batchInfo := batchInfos[batchID]
		batchInfo.CurrentHash = lastBlock.CurrentHash
		batchInfos[batchID] = batchInfo

		// 更新数据库中的批次信息
		_, err := db.Exec(`UPDATE batch_info SET current_hash = ? WHERE batch_id = ?`,
			batchInfo.CurrentHash, batchID)
		if err != nil {
			return err
		}
	}

	// 更新数据库中的区块信息
	_, err := db.Exec(`UPDATE product_chain SET detection_hash = ?, current_hash = ? WHERE batch_id = ? AND level = ?`,
		detectionHash, targetBlock.CurrentHash, batchID, level)
	if err != nil {
		return err
	}

	// 更新后续区块的哈希
	for i := targetBlock.Index + 1; i < len(chain.Chain); i++ {
		_, err := db.Exec(`UPDATE product_chain SET previous_hash = ?, current_hash = ? WHERE batch_id = ? AND block_index = ?`,
			chain.Chain[i].PreviousHash, chain.Chain[i].CurrentHash, batchID, chain.Chain[i].Index)
		if err != nil {
			return err
		}
	}

	saveBlockchains()

	// 如果是主节点，更新检测结果后向其他主节点发送同步请求
	if isMainNode {
		go syncBlockchainWithMainNodes()
	}

	return nil
}

// 添加处理记录
func addProcessingHandler(w http.ResponseWriter, r *http.Request) {
	// 从上下文获取用户信息
	userVal := r.Context().Value("user")
	if userVal == nil {
		sendErrorResponse(w, "未授权访问", http.StatusUnauthorized, "UNAUTHORIZED")
		return
	}

	user, ok := userVal.(User)
	if !ok {
		sendErrorResponse(w, "无效的用户信息", http.StatusInternalServerError, "INVALID_USER_DATA")
		return
	}

	// 1. 解析请求数据
	var request struct {
		BatchID     string      `json:"batch_id"`
		Level       int         `json:"level"`
		ProductInfo ProductInfo `json:"product_info"`
	}

	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		sendErrorResponse(w, fmt.Sprintf("JSON解析失败: %v", err), http.StatusBadRequest, "INVALID_JSON")
		return
	}

	// 2. 验证基础字段
	if err := validateProductInfo(request.ProductInfo); err != nil {
		sendErrorResponse(w, err.Error(), http.StatusBadRequest, "INVALID_PRODUCT_INFO")
		return
	}

	// 3. 检查批次是否存在
	if _, exists := blockchains[request.BatchID]; !exists {
		sendErrorResponse(w, "批次不存在", http.StatusNotFound, "BATCH_NOT_FOUND")
		return
	}

	// 4. 检查批次是否已完成
	if batchInfos[request.BatchID].Complete {
		sendErrorResponse(w, "此批次已完成全部处理流程", http.StatusBadRequest, "BATCH_COMPLETED")
		return
	}

	// 5. 添加区块到链
	block, err := addBlockToChain(request.BatchID, request.Level, request.ProductInfo, user)
	if err != nil {
		log.Printf("添加区块失败: %v", err)
		sendErrorResponse(w, err.Error(), http.StatusBadRequest, "BLOCK_ADD_FAILED")
		return
	}

	// 6. 如果是最后阶段生成QR码
	if request.Level >= maxProcessLevel-1 {
		if err := generateQRCode(block.ProductInfo.BatchID, block.CurrentHash); err != nil {
			log.Printf("生成QR码失败: %v (批次: %s, 区块哈希: %s)", err, block.ProductInfo.BatchID, block.CurrentHash)
		}
	}

	// 7. 返回成功响应
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ProcessingResponse{
		Success:    true,
		BatchID:    block.ProductInfo.BatchID,
		BlockIndex: block.Index,
		BlockHash:  block.CurrentHash,
		PrevHash:   block.PreviousHash,
		Timestamp:  block.Timestamp,
	})
}

// 获取级别描述
func getLevelDescription(level int) string {
	descriptions := map[int]string{
		0: "批次注册",
		1: "原材料登记",
		2: "初级加工",
		3: "质量检测",
		4: "深度加工",
		5: "加工完成",
		6: "物流运输",
		7: "零售终端",
	}
	if desc, exists := descriptions[level]; exists {
		return desc
	}
	return fmt.Sprintf("自定义处理阶段 (级别 %d)", level)
}

func getProductChainHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	batchID := vars["batch_id"]
	if batchID == "" {
		sendErrorResponse(w, "Missing batch_id parameter", http.StatusBadRequest, "MISSING_BATCH_ID")
		return
	}

	chain, exists := blockchains[batchID]
	if !exists {
		sendErrorResponse(w, "批次不存在", http.StatusNotFound, "BATCH_NOT_FOUND")
		return
	}

	// 准备模板数据
	type ProcessStep struct {
		Description   string
		ProcessorID   string
		ProcessDate   string
		Details       string
		Note          string
		ImageHash     string
		ImageURL      string
		DetectionHash string
		DetectionURL  string
		Level         int
		Location      string
		QualityCheck  string
	}

	// 获取批次信息
	batchInfo, exists := batchInfos[batchID]
	if !exists {
		sendErrorResponse(w, "批次信息不存在", http.StatusNotFound, "BATCH_INFO_NOT_FOUND")
		return
	}

	// 获取当前年份
	currentYear := time.Now().Year()

	data := struct {
		BatchID      string
		ProductType  string
		CreationDate string
		CurrentLevel int
		MaxLevel     int
		Complete     bool
		CurrentYear  int
		ProcessSteps []ProcessStep
	}{
		BatchID:      batchID,
		ProductType:  batchInfo.ProductType,
		CreationDate: batchInfo.CreationDate.Format("2006-01-02"),
		CurrentLevel: batchInfo.CurrentLevel,
		MaxLevel:     maxProcessLevel,
		Complete:     batchInfo.Complete,
		CurrentYear:  currentYear,
	}

	// 填充处理步骤信息
	for _, block := range chain.Chain {
		if block.Level < 0 {
			continue
		}

		// 构建图片URL（如果存在）
		imageURL := ""
		if block.ProductInfo.ImageHash != "" {
			imageExts := []string{".png", ".jpg", ".jpeg", ".gif"}
			for _, ext := range imageExts {
				imagePath := filepath.Join(imagesDir, block.ProductInfo.ImageHash+ext)
				if _, err := os.Stat(imagePath); err == nil {
					imageURL = fmt.Sprintf("/images/%s%s", block.ProductInfo.ImageHash, ext)
					break
				}
			}
		}

		// 检测功能已移除，不再处理检测结果图片URL
		detectionURL := "" // 检测功能已移除，始终返回空URL

		data.ProcessSteps = append(data.ProcessSteps, ProcessStep{
			Description:   getLevelDescription(block.Level),
			ProcessorID:   block.ProductInfo.ProcessorID,
			ProcessDate:   block.ProductInfo.ProcessDate,
			Details:       block.ProductInfo.Details,
			Note:          block.ProductInfo.Note,
			ImageHash:     block.ProductInfo.ImageHash,
			ImageURL:      imageURL,
			DetectionHash: block.ProductInfo.DetectionHash,
			DetectionURL:  detectionURL,
			Level:         block.Level,
			Location:      block.ProductInfo.Location,
			QualityCheck:  block.ProductInfo.QualityCheck,
		})
	}

	// 解析并执行模板 - 内嵌模板
	const traceTemplate = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>产品溯源信息 - {{.BatchID}}</title>
    <style>
        :root {
            --primary: #2563eb; /* 主色调 - 蓝色 */
            --primary-light: #dbeafe; /* 主色调浅色 */
            --secondary: #16a34a; /* 辅助色 - 绿色 */
            --secondary-light: #dcfce7; /* 辅助色浅色 */
            --danger: #dc2626; /* 危险色 - 红色 */
            --warning: #f59e0b; /* 警告色 - 橙色 */
            --text: #1f2937; /* 文本色 */
            --text-light: #6b7280; /* 文本浅色 */
            --bg: #ffffff; /* 背景色 */
            --bg-light: #f9fafb; /* 背景浅色 */
            --border: #e5e7eb; /* 边框色 */
            --shadow: 0 4px 6px -1px rgba(0, 0, 0, 0.1), 0 2px 4px -1px rgba(0, 0, 0, 0.06);
            --shadow-lg: 0 10px 15px -3px rgba(0, 0, 0, 0.1), 0 4px 6px -2px rgba(0, 0, 0, 0.05);
            --radius: 0.5rem; /* 圆角半径 */
            --transition: all 0.3s ease;
        }

        * {
            margin: 0;
            padding: 0;
            box-sizing: border-box;
        }

        body {
            font-family: 'Segoe UI', Roboto, 'Helvetica Neue', Arial, sans-serif;
            color: var(--text);
            background-color: var(--bg-light);
            line-height: 1.6;
            padding: 0;
            margin: 0;
        }

        .container {
            max-width: 1200px;
            margin: 0 auto;
            padding: 2rem 1.5rem;
        }

        header {
            background-color: var(--bg);
            box-shadow: var(--shadow);
            padding: 1.5rem 0;
            position: sticky;
            top: 0;
            z-index: 10;
        }

        .header-content {
            max-width: 1200px;
            margin: 0 auto;
            padding: 0 1.5rem;
            display: flex;
            justify-content: space-between;
            align-items: center;
        }

        .logo {
            font-size: 1.5rem;
            font-weight: 700;
            color: var(--primary);
            display: flex;
            align-items: center;
            gap: 0.5rem;
        }

        .logo svg {
            width: 2rem;
            height: 2rem;
        }

        h1 {
            color: var(--text);
            text-align: center;
            margin-bottom: 2rem;
            font-size: 1.875rem;
            font-weight: 700;
        }

        .batch-info-card {
            background-color: var(--bg);
            border-radius: var(--radius);
            box-shadow: var(--shadow);
            padding: 1.5rem;
            margin-bottom: 2rem;
            border-left: 4px solid var(--primary);
        }

        .batch-info-grid {
            display: grid;
            grid-template-columns: repeat(auto-fill, minmax(200px, 1fr));
            gap: 1rem;
        }

        .info-item {
            margin-bottom: 0.5rem;
        }

        .info-label {
            font-size: 0.875rem;
            color: var(--text-light);
            margin-bottom: 0.25rem;
        }

        .info-value {
            font-weight: 600;
        }

        .status-badge {
            display: inline-flex;
            align-items: center;
            padding: 0.25rem 0.75rem;
            border-radius: 9999px;
            font-size: 0.75rem;
            font-weight: 600;
        }

        .status-badge.completed {
            background-color: var(--secondary-light);
            color: var(--secondary);
        }

        .status-badge.in-progress {
            background-color: var(--primary-light);
            color: var(--primary);
        }

        .timeline {
            position: relative;
            max-width: 1200px;
            margin: 0 auto;
            padding: 2rem 0;
        }

        .timeline::after {
            content: '';
            position: absolute;
            width: 4px;
            background-color: var(--border);
            top: 0;
            bottom: 0;
            left: 50%;
            margin-left: -2px;
            transition: var(--transition);
        }

        @media (max-width: 768px) {
            .timeline::after {
                left: 31px;
            }
        }

        .process-step {
            padding: 10px 40px;
            position: relative;
            width: 50%;
            box-sizing: border-box;
            transition: var(--transition);
        }

        .process-step:nth-child(odd) {
            left: 0;
        }

        .process-step:nth-child(even) {
            left: 50%;
        }

        @media (max-width: 768px) {
            .process-step {
                width: 100%;
                padding-left: 70px;
                padding-right: 25px;
            }

            .process-step:nth-child(even) {
                left: 0;
            }
        }

        .step-card {
            background-color: var(--bg);
            border-radius: var(--radius);
            box-shadow: var(--shadow);
            padding: 1.5rem;
            transition: var(--transition);
            position: relative;
        }

        .step-card:hover {
            transform: translateY(-5px);
            box-shadow: var(--shadow-lg);
        }

        .step-date {
            font-size: 0.875rem;
            color: var(--text-light);
            margin-bottom: 0.5rem;
        }

        .step-title {
            font-size: 1.25rem;
            font-weight: 700;
            color: var(--primary);
            margin-bottom: 1rem;
            display: flex;
            align-items: center;
            gap: 0.5rem;
        }

        .step-level {
            display: inline-flex;
            align-items: center;
            justify-content: center;
            width: 2rem;
            height: 2rem;
            background-color: var(--primary-light);
            color: var(--primary);
            border-radius: 50%;
            font-size: 0.875rem;
            font-weight: 700;
        }

        .step-meta {
            display: flex;
            gap: 1rem;
            margin-bottom: 1rem;
            font-size: 0.875rem;
            color: var(--text-light);
        }

        .step-content {
            margin-top: 1rem;
        }

        .detail-item {
            margin-bottom: 0.75rem;
        }

        .detail-label {
            font-weight: 600;
            display: block;
            margin-bottom: 0.25rem;
        }

        .note-section {
            margin-top: 1rem;
            padding: 1rem;
            background-color: #fef3c7;
            border-radius: var(--radius);
            border-left: 3px solid var(--warning);
        }

        .note-label {
            font-weight: 700;
            color: #92400e;
            margin-bottom: 0.25rem;
        }

        .image-gallery {
            margin-top: 1.5rem;
            display: grid;
            grid-template-columns: repeat(auto-fill, minmax(250px, 1fr));
            gap: 1rem;
        }

        .image-container {
            position: relative;
            overflow: hidden;
            border-radius: var(--radius);
            box-shadow: var(--shadow);
            aspect-ratio: 1/1;
            transition: var(--transition);
        }

        .image-container:hover {
            transform: scale(1.03);
            box-shadow: var(--shadow-lg);
        }

        .step-image {
            width: 100%;
            height: 100%;
            object-fit: cover;
            transition: var(--transition);
        }

        .image-overlay {
            position: absolute;
            bottom: 0;
            left: 0;
            right: 0;
            background: linear-gradient(to top, rgba(0,0,0,0.7) 0%, rgba(0,0,0,0) 100%);
            padding: 1rem;
            color: white;
            transform: translateY(100%);
            transition: var(--transition);
        }

        .image-container:hover .image-overlay {
            transform: translateY(0);
        }

        .image-caption {
            font-size: 0.875rem;
            font-weight: 600;
        }

        .detection-container {
            margin-top: 1.5rem;
            padding: 1rem;
            background-color: var(--secondary-light);
            border-radius: var(--radius);
            border-left: 3px solid var(--secondary);
        }

        .detection-title {
            font-weight: 700;
            color: var(--secondary);
            margin-bottom: 1rem;
            text-align: center;
        }

        .no-image {
            text-align: center;
            padding: 2rem;
            color: var(--text-light);
            background-color: #f3f4f6;
            border-radius: var(--radius);
        }

        .modal {
            display: none;
            position: fixed;
            z-index: 1000;
            left: 0;
            top: 0;
            width: 100%;
            height: 100%;
            overflow: auto;
            background-color: rgba(0,0,0,0.95);
            opacity: 0;
            transition: opacity 0.3s ease;
        }

        .modal.active {
            opacity: 1;
        }

        .modal-content {
            margin: auto;
            display: block;
            max-width: 90%;
            max-height: 90%;
            transform: scale(0.9);
            transition: transform 0.3s ease;
        }

        .modal.active .modal-content {
            transform: scale(1);
        }

        .close {
            position: absolute;
            top: 1.5rem;
            right: 2rem;
            color: white;
            font-size: 2.5rem;
            font-weight: bold;
            cursor: pointer;
            transition: var(--transition);
            z-index: 1001;
        }

        .close:hover {
            color: var(--primary);
            transform: rotate(90deg);
        }

        .footer {
            background-color: var(--bg);
            padding: 2rem 0;
            margin-top: 3rem;
            border-top: 1px solid var(--border);
        }

        .footer-content {
            max-width: 1200px;
            margin: 0 auto;
            padding: 0 1.5rem;
            text-align: center;
            color: var(--text-light);
            font-size: 0.875rem;
        }
    </style>
</head>
<body>
    <header>
        <div class="header-content">
            <div class="logo">
                <svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                    <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9 12l2 2 4-4m5.618-4.016A11.955 11.955 0 0112 2.944a11.955 11.955 0 01-8.618 3.04A12.02 12.02 0 003 9c0 5.591 3.824 10.29 9 11.622 5.176-1.332 9-6.03 9-11.622 0-1.042-.133-2.052-.382-3.016z" />
                </svg>
                产品溯源系统
            </div>
        </div>
    </header>

    <div class="container">
        <h1>产品溯源信息</h1>

        <div class="batch-info-card">
            <div class="batch-info-grid">
                <div class="info-item">
                    <div class="info-label">批次ID</div>
                    <div class="info-value">{{.BatchID}}</div>
                </div>
                <div class="info-item">
                    <div class="info-label">产品类型</div>
                    <div class="info-value">{{.ProductType}}</div>
                </div>
                <div class="info-item">
                    <div class="info-label">创建日期</div>
                    <div class="info-value">{{.CreationDate}}</div>
                </div>
                <div class="info-item">
                    <div class="info-label">当前级别</div>
                    <div class="info-value">{{.CurrentLevel}} / {{.MaxLevel}}</div>
                </div>
                <div class="info-item">
                    <div class="info-label">状态</div>
                    <div class="info-value">
                        {{if .Complete}}
                        <span class="status-badge completed">已完成</span>
                        {{else}}
                        <span class="status-badge in-progress">进行中</span>
                        {{end}}
                    </div>
                </div>
            </div>
        </div>

        <div class="timeline">
            {{range .ProcessSteps}}
            <div class="process-step">
                <div class="step-card">
                    <div class="step-date">{{.ProcessDate}}</div>
                    <div class="step-title">
                        <span class="step-level">{{.Level}}</span>
                        {{.Description}}
                    </div>
                    <div class="step-meta">
                        <div>处理单位: {{.ProcessorID}}</div>
                        <div>位置: {{.Location}}</div>
                    </div>

                    <div class="step-content">
                        <div class="detail-item">
                            <div class="detail-label">处理详情</div>
                            <div>{{.Details}}</div>
                        </div>

                        <div class="detail-item">
                            <div class="detail-label">质量检测</div>
                            <div>{{.QualityCheck}}</div>
                        </div>

                        {{if .Note}}
                        <div class="note-section">
                            <div class="note-label">备注</div>
                            <div>{{.Note}}</div>
                        </div>
                        {{end}}

                        

                        {{if .DetectionURL}}
                        <div class="detection-container">
                            <div class="detection-title">检测结果图片</div>
                            <div class="image-container" onclick="openImageModal('{{.DetectionURL}}')">
                                <img src="{{.DetectionURL}}" alt="检测结果图片" class="step-image">
                                <div class="image-overlay">
                                    <div class="image-caption">查看大图</div>
                                </div>
                            </div>
                        </div>
                        {{end}}
                    </div>
                </div>
            </div>
            {{end}}
        </div>
    </div>

    <footer class="footer">
        <div class="footer-content">
            <p>产品溯源系统 &copy; {{.CurrentYear}} 版权所有</p>
        </div>
    </footer>

    <!-- 图片查看模态框 -->
    <div id="imageModal" class="modal">
        <span class="close">&times;</span>
        <img class="modal-content" id="modalImage">
    </div>

    <script>
        // 图片查看模态框功能
        var modal = document.getElementById("imageModal");
        var modalImg = document.getElementById("modalImage");
        var span = document.getElementsByClassName("close")[0];
        var isDragging = false;
        var startX, startY, scrollLeft, scrollTop;

        function openImageModal(src) {
            modal.style.display = "block";
            // 添加延迟以启用过渡效果
            setTimeout(function() {
                modal.classList.add("active");
            }, 10);
            modalImg.src = src;
            document.body.style.overflow = "hidden"; // 防止背景滚动
        }

        function closeModal() {
            modal.classList.remove("active");
            // 等待过渡效果完成后隐藏
            setTimeout(function() {
                modal.style.display = "none";
                document.body.style.overflow = "auto"; // 恢复背景滚动
            }, 300);
        }

        span.onclick = closeModal;

        // 点击模态框外部关闭
        modal.onclick = function(event) {
            if (event.target === modal) {
                closeModal();
            }
        };

        // 键盘Esc关闭
        document.addEventListener("keydown", function(event) {
            if (event.key === "Escape" && modal.style.display === "block") {
                closeModal();
            }
        });

        // 图片拖动功能（适用于大屏幕图片）
        modalImg.onmousedown = function(e) {
            isDragging = true;
            startX = e.pageX - modalImg.offsetLeft;
            startY = e.pageY - modalImg.offsetTop;
            scrollLeft = modal.scrollLeft;
            scrollTop = modal.scrollTop;
            modalImg.style.cursor = "grabbing";
        };

        modalImg.onmouseleave = function() {
            isDragging = false;
            modalImg.style.cursor = "pointer";
        };

        modalImg.onmouseup = function() {
            isDragging = false;
            modalImg.style.cursor = "pointer";
        };

        modalImg.onmousemove = function(e) {
            if (!isDragging) return;
            e.preventDefault();
            var x = e.pageX - modalImg.offsetLeft;
            var y = e.pageY - modalImg.offsetTop;
            var walkX = (x - startX) * 1; // 拖动速度
            var walkY = (y - startY) * 1;
            modal.scrollLeft = scrollLeft - walkX;
            modal.scrollTop = scrollTop - walkY;
        };
    </script>
</body>
</html>`
	tmpl, err := template.New("trace").Parse(traceTemplate)
	if err != nil {
		sendErrorResponse(w, "生成页面错误", http.StatusInternalServerError, "TEMPLATE_ERROR")
		return
	}

	w.Header().Set("Content-Type", "text/html")
	tmpl.Execute(w, data)
}

func generateQRCode(batchID, hash string) error {
	qrDir := filepath.Join(dataDir, "qrcodes")
	if err := os.MkdirAll(qrDir, 0755); err != nil {
		return err
	}

	url := fmt.Sprintf("http://%s/product/%s", getServerAddress(), batchID)
	return qrcode.WriteFile(url, qrcode.Medium, 256, filepath.Join(qrDir, batchID+".png"))
}

func getServerAddress() string {
	if os.Getenv("ENV") == "production" {
		return "your-production-domain.com"
	}
	return "111.230.7.188:8090"
}

func getQRCodeHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	batchID := vars["batch_id"]
	if batchID == "" {
		sendErrorResponse(w, "Missing batch_id parameter", http.StatusBadRequest, "MISSING_BATCH_ID")
		return
	}

	// 检查是否需要刷新QR码
	refresh := r.URL.Query().Get("refresh") == "true"

	qrPath := filepath.Join(dataDir, "qrcodes", batchID+".png")

	// 如果需要刷新或QR码不存在，则生成新的QR码
	if refresh || func() bool {
		_, err := os.Stat(qrPath)
		return os.IsNotExist(err)
	}() {
		// 获取最新的区块哈希
		var latestHash string
		if chain, exists := blockchains[batchID]; exists && len(chain.Chain) > 0 {
			latestHash = chain.Chain[len(chain.Chain)-1].CurrentHash
		}

		// 生成QR码
		if err := generateQRCode(batchID, latestHash); err != nil {
			sendErrorResponse(w, "Failed to generate QR code", http.StatusInternalServerError, "QR_GENERATION_FAILED")
			return
		}
	}
	http.ServeFile(w, r, qrPath)
}

func listBatchesHandler(w http.ResponseWriter, r *http.Request) {
	var batches []BatchInfo
	for _, info := range batchInfos {
		if info.BatchID != "system" { // 排除系统批次
			batches = append(batches, info)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(batches)
}

func healthCheckHandler(w http.ResponseWriter, r *http.Request) {
	if err := db.Ping(); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// YOLO服务回调处理函数（ai辅助开发@doubao）
// 已移除yoloDetectionCallbackHandler函数（不再需要YOLO检测回调处理）
func yoloDetectionCallbackHandler(w http.ResponseWriter, r *http.Request) { // 保留函数但不执行任何操作
	// 此函数已被移除，仅保留接口兼容性
	// 处理预检请求
	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusOK)
		return
	}

	// 设置CORS头并返回空响应
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "YOLO检测功能已移除",
	})
	return
}

// 注意：downloadDetectionImage函数的具体实现移至第3554行

// 处理检测结果上传
func uploadDetectionHandler(w http.ResponseWriter, r *http.Request) {
	// 确保只接受POST请求
	if r.Method != http.MethodPost {
		sendErrorResponse(w, "只支持POST请求", http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED")
		return
	}

	// 解析多部分表单数据
	err := r.ParseMultipartForm(10 << 20) // 10MB
	if err != nil {
		sendErrorResponse(w, "解析表单数据失败", http.StatusBadRequest, "INVALID_FORM_DATA")
		return
	}

	// 获取批次ID
	batchID := r.FormValue("batch_id")
	if batchID == "" {
		sendErrorResponse(w, "批次ID不能为空", http.StatusBadRequest, "MISSING_BATCH_ID")
		return
	}

	// 获取检测结果JSON字符串
	detectionJSON := r.FormValue("detection_result")
	if detectionJSON == "" {
		sendErrorResponse(w, "检测结果不能为空", http.StatusBadRequest, "MISSING_DETECTION_RESULT")
		return
	}

	// 解析检测结果JSON
	var detectionResult map[string]interface{}
	err = json.Unmarshal([]byte(detectionJSON), &detectionResult)
	if err != nil {
		sendErrorResponse(w, "解析检测结果JSON失败", http.StatusBadRequest, "INVALID_JSON")
		return
	}

	// 处理上传的图像（如果有）
	var imageHash string
	file, header, err := r.FormFile("image")
	if err == nil {
		defer file.Close()

		// 创建检测图片目录
		detectionDir := filepath.Join(imagesDir, "detection")
		os.MkdirAll(detectionDir, 0755)

		// 生成唯一的文件名
		ext := filepath.Ext(header.Filename)
		if ext == "" {
			ext = ".jpg"
		}
		imageFilename := fmt.Sprintf("%s_%d%s", batchID, time.Now().UnixNano(), ext)
		imagePath := filepath.Join(detectionDir, imageFilename)

		// 保存文件
		dst, err := os.Create(imagePath)
		if err != nil {
			sendErrorResponse(w, "创建图片文件失败", http.StatusInternalServerError, "FILE_CREATION_FAILED")
			return
		}
		defer dst.Close()

		// 复制文件内容
		_, err = io.Copy(dst, file)
		if err != nil {
			sendErrorResponse(w, "保存图片文件失败", http.StatusInternalServerError, "FILE_SAVE_FAILED")
			return
		}

		// 计算图片哈希
		imageHash, err = calculateFileHash(imagePath)
		if err != nil {
			// 哈希计算失败不影响主要功能，只记录日志
			log.Printf("计算图片哈希失败: %v", err)
		}
	}

	// 记录接收到的检测结果
	log.Printf("收到检测结果 - 批次ID: %s, 检测到的对象数量: %v, 图片哈希: %s",
		batchID,
		getDetectionsCount(detectionResult),
		imageHash)

	// 检查批次是否存在
	if _, exists := batchInfos[batchID]; !exists {
		// 如果批次不存在，创建一个新批次
		// 这里我们创建一个临时用户用于处理自动创建的批次
		tempUser := User{
			Username:      "system",
			Role:          "admin",
			AllowedLevels: []int{0, 1, 2, 3, 4, 5, 6, 7},
			IsActive:      true,
		}

		err = createNewBatch(batchID, tempUser)
		if err != nil {
			sendErrorResponse(w, "创建新批次失败: "+err.Error(), http.StatusInternalServerError, "BATCH_CREATION_FAILED")
			return
		}
	}

	// 为检测结果创建一个新的区块
	// 假设这是质量检测步骤（级别2）
	level := 2

	// 从检测结果中提取质量信息
	qualityCheck := "通过"
	if detections, ok := detectionResult["detections"].([]interface{}); ok {
		// 这里可以根据检测结果判断质量是否合格
		// 简单示例：如果检测到的对象超过某个阈值，则标记为不合格
		if len(detections) > 10 {
			qualityCheck = "需要审核"
		}
	}

	// 创建产品信息
	productInfo := ProductInfo{
		BatchID:       batchID,
		ProcessorID:   "yolo_detection_system",
		ProcessDate:   time.Now().Format("2006-01-02"),
		Details:       "自动YOLO检测处理",
		QualityCheck:  qualityCheck,
		Location:      "检测实验室",
		Note:          "自动生成的检测记录",
		ImageHash:     imageHash,
		DetectionHash: imageHash, // 检测结果图片哈希
	}

	// 为了添加区块，我们需要一个用户
	tempUser := User{
		Username:      "system",
		Role:          "admin",
		AllowedLevels: []int{0, 1, 2, 3, 4, 5, 6, 7},
		IsActive:      true,
	}

	// 添加区块到区块链
	block, err := addBlockToChain(batchID, level, productInfo, tempUser)
	if err != nil {
		sendErrorResponse(w, "添加区块失败: "+err.Error(), http.StatusInternalServerError, "BLOCK_ADD_FAILED")
		return
	}

	// 返回成功响应
	response := ProcessingResponse{
		Success:    true,
		BatchID:    batchID,
		BlockIndex: block.Index,
		BlockHash:  block.CurrentHash,
		PrevHash:   block.PreviousHash,
		Timestamp:  block.Timestamp,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// 辅助函数：获取检测到的对象数量
func getDetectionsCount(detectionResult map[string]interface{}) int {
	if detections, ok := detectionResult["detections"].([]interface{}); ok {
		return len(detections)
	}
	return 0
}

// 此函数已被移除，仅保留接口兼容性
// 	if r.Method == "OPTIONS" {
// 		w.WriteHeader(http.StatusOK)
// 		return
// 	}
// 	// 设置CORS头并返回空响应
// 	w.Header().Set("Content-Type", "application/json")
// 	w.Header().Set("Access-Control-Allow-Origin", "*")
// 	w.WriteHeader(http.StatusOK)
// 	json.NewEncoder(w).Encode(map[string]interface{}{
// 		"success": true,
// 		"message": "检测结果上传功能已移除",
// 	})
// 	return
// }

// 已移除detectionHandler函数（不再需要检测结果图片服务）
func detectionHandler(w http.ResponseWriter, r *http.Request) { // 保留函数但不执行任何操作
	// 此函数已被移除，仅保留接口兼容性
	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusOK)
		return
	}
	// 设置CORS头并返回空响应
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusNotFound)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": false,
		"message": "检测结果图片服务已移除",
	})
	return
}

// 函数已完全移除，仅保留空壳接口

// 获取节点信息的HTTP处理器
func getNodeInfoHandler(w http.ResponseWriter, r *http.Request) {
	// 设置响应头
	w.Header().Set("Content-Type", "application/json")

	// 构建响应数据
	response := map[string]interface{}{
		"success":      true,
		"node_id":      thisNodeID,
		"is_main_node": isMainNode,
		"p2p_port":     p2pPort,
		"api_port":     8080,
		"network_size": len(knownNodes),
		"blockchain_stats": map[string]interface{}{
			"total_batches": len(blockchains),
		},
		"version": "1.0.0",
	}

	// 发送响应
	json.NewEncoder(w).Encode(response)
}

// 获取节点列表的HTTP处理器
func getNodeListHandler(w http.ResponseWriter, r *http.Request) {
	// 设置响应头
	w.Header().Set("Content-Type", "application/json")

	// 收集节点信息和负载数据
	loadInfoMutex.RLock()
	p2pMutex.RLock()

	var nodes []map[string]interface{}
	for _, node := range knownNodes {
		// 跳过当前节点和非主节点
		if node.ID == thisNodeID || !node.IsMainNode {
			continue
		}

		// 构建节点信息
		nodeInfo := map[string]interface{}{
			"node_id":      node.ID,
			"address":      node.Address,
			"is_main_node": node.IsMainNode,
			"last_seen":    node.LastSeen,
		}

		// 添加负载信息
		if loadInfo, exists := loadBalancingInfo[node.ID]; exists {
			nodeInfo["load_info"] = loadInfo
		}

		nodes = append(nodes, nodeInfo)
	}

	// 添加当前节点信息
	currentNodeInfo := map[string]interface{}{
		"node_id":      thisNodeID,
		"address":      fmt.Sprintf("%s:%d", getLocalIP(), 8080),
		"is_main_node": isMainNode,
		"last_seen":    time.Now().Unix(),
	}

	// 获取当前节点的负载信息
	currentLoadInfo := getCurrentNodeLoadInfo()
	currentNodeInfo["load_info"] = currentLoadInfo
	nodes = append(nodes, currentNodeInfo)

	p2pMutex.RUnlock()
	loadInfoMutex.RUnlock()

	// 构建响应数据
	response := map[string]interface{}{
		"success":     true,
		"nodes":       nodes,
		"total_nodes": len(nodes),
	}

	// 发送响应
	json.NewEncoder(w).Encode(response)
}

// 获取最佳节点的HTTP处理器
func getBestNodeHandler(w http.ResponseWriter, r *http.Request) {
	// 设置响应头
	w.Header().Set("Content-Type", "application/json")

	// 选择最佳节点
	bestNode := selectBestNode()
	if bestNode == nil {
		sendErrorResponse(w, "没有可用的节点", http.StatusServiceUnavailable, "NO_AVAILABLE_NODES")
		return
	}

	// 构建响应数据
	response := map[string]interface{}{
		"success":   true,
		"best_node": bestNode,
	}

	// 发送响应
	json.NewEncoder(w).Encode(response)
}

// 根据负载均衡算法选择最佳节点
func selectBestNode() map[string]interface{} {
	loadInfoMutex.RLock()
	p2pMutex.RLock()
	defer loadInfoMutex.RUnlock()
	defer p2pMutex.RUnlock()

	var bestNode map[string]interface{}
	var bestScore float64 = -1
	currentTime := time.Now().Unix()

	// 检查当前节点
	currentNodeIP := getLocalIP()
	currentNodeAddr := fmt.Sprintf("%s:%d", currentNodeIP, 8080)
	currentLoadInfo := getCurrentNodeLoadInfo()
	currentScore := calculateNodeScore(currentLoadInfo, currentTime, true)

	bestNode = map[string]interface{}{
		"node_id":      thisNodeID,
		"address":      currentNodeAddr,
		"is_main_node": isMainNode,
		"load_info":    currentLoadInfo,
		"score":        currentScore,
	}
	bestScore = currentScore

	// 检查其他主节点
	for _, node := range knownNodes {
		if node.ID == thisNodeID || !node.IsMainNode {
			continue
		}

		// 构建节点地址
		ip := strings.Split(node.Address, ":")[0]
		nodeAddr := fmt.Sprintf("%s:%d", ip, 8080)

		// 获取负载信息
		loadInfo, exists := loadBalancingInfo[node.ID]
		if !exists {
			// 如果没有负载信息，使用默认值
			loadInfo = NodeLoadInfo{
				CPUUsage:       50.0,
				MemoryUsage:    50.0,
				ResponseTime:   500,
				RequestCount:   10,
				LastUpdateTime: currentTime,
			}
		}

		// 计算节点分数
		nodeScore := calculateNodeScore(loadInfo, currentTime, false)

		// 更新最佳节点
		if nodeScore > bestScore {
			bestScore = nodeScore
			bestNode = map[string]interface{}{
				"node_id":      node.ID,
				"address":      nodeAddr,
				"is_main_node": node.IsMainNode,
				"load_info":    loadInfo,
				"score":        nodeScore,
			}
		}
	}

	return bestNode
}

// 计算节点分数
func calculateNodeScore(loadInfo NodeLoadInfo, currentTime int64, isLocalNode bool) float64 {
	// 检查节点是否活跃（5分钟内有更新）
	if currentTime-loadInfo.LastUpdateTime > 300 {
		return 0.1 // 不活跃节点分数低
	}

	// 计算各指标的归一化分数（值越低越好）
	cpuScore := 1.0 - (loadInfo.CPUUsage / 100.0)
	memoryScore := 1.0 - (loadInfo.MemoryUsage / 100.0)
	responseTimeScore := 1.0 - min(float64(loadInfo.ResponseTime)/2000.0, 1.0) // 假设2000ms为上限
	requestCountScore := 1.0 - min(float64(loadInfo.RequestCount)/100.0, 1.0)  // 假设100个请求为上限

	// 加权平均分数
	totalScore := (cpuScore*0.3 + memoryScore*0.2 + responseTimeScore*0.3 + requestCountScore*0.2)

	// 本地节点加分
	if isLocalNode {
		totalScore += 0.1
	}

	// 确保分数在合理范围内
	return max(0.1, min(1.0, totalScore))
}

// 获取当前节点的负载信息
func getCurrentNodeLoadInfo() NodeLoadInfo {
	// 这里简化实现，实际应用中应该收集真实的系统负载信息
	return NodeLoadInfo{
		CPUUsage:       30.0, // 示例值
		MemoryUsage:    40.0, // 示例值
		ResponseTime:   200,  // 示例值(ms)
		RequestCount:   5,    // 示例值
		LastUpdateTime: time.Now().Unix(),
	}
}

// 处理剪枝节点同步请求
func handlePrunedSyncRequest(w http.ResponseWriter, r *http.Request) {
	// 验证请求方法
	if r.Method != http.MethodPost {
		sendErrorResponse(w, "只支持POST请求", http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED")
		return
	}

	// 从上下文获取用户信息
	userVal := r.Context().Value("user")
	if userVal == nil {
		sendErrorResponse(w, "未授权访问", http.StatusUnauthorized, "UNAUTHORIZED")
		return
	}

	// 异步执行剪枝节点同步，避免阻塞HTTP响应
	go func() {
		log.Println("开始剪枝节点同步...")
		// 对所有批次执行完整同步
		syncMutex.RLock()
		batchIDs := make([]string, 0, len(blockchains))
		for batchID := range blockchains {
			batchIDs = append(batchIDs, batchID)
		}
		syncMutex.RUnlock()

		// 向网络中的所有子节点广播同步请求
		sendBlockchainSyncToAllNodes(batchIDs)
		log.Println("剪枝节点同步已开始")
	}()

	// 返回成功响应
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "剪枝节点同步完成",
	})
}

// 处理轻节点同步请求
func handleLightSyncRequest(w http.ResponseWriter, r *http.Request) {
	// 验证请求方法
	if r.Method != http.MethodPost {
		sendErrorResponse(w, "只支持POST请求", http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED")
		return
	}

	// 从上下文获取用户信息
	userVal := r.Context().Value("user")
	if userVal == nil {
		sendErrorResponse(w, "未授权访问", http.StatusUnauthorized, "UNAUTHORIZED")
		return
	}

	// 异步执行轻节点同步，避免阻塞HTTP响应
	go func() {
		log.Println("开始轻节点同步...")
		// 向网络中的所有子节点广播轻量级同步请求
		sendBlockchainLightSyncToAllNodes()
		log.Println("轻节点同步已开始")
	}()

	// 返回成功响应
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "轻节点同步完成",
	})
}

// 手动触发主节点间同步的处理函数
func manualSyncHandler(w http.ResponseWriter, r *http.Request) {
	// 验证请求方法
	if r.Method != http.MethodPost {
		sendErrorResponse(w, "只支持POST请求", http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED")
		return
	}

	// 从上下文获取用户信息
	userVal := r.Context().Value("user")
	if userVal == nil {
		sendErrorResponse(w, "未授权访问", http.StatusUnauthorized, "UNAUTHORIZED")
		return
	}

	user := userVal.(User)
	// 检查用户权限 - 只有管理员才能手动触发同步
	if user.Role != "admin" {
		sendErrorResponse(w, "只有管理员才能触发同步", http.StatusForbidden, "FORBIDDEN")
		return
	}

	// 异步执行主节点间同步，避免阻塞HTTP响应
	go func() {
		log.Printf("用户 %s 手动触发了主节点间同步...", user.Username)
		syncBlockchainWithMainNodes()
		log.Println("主节点间同步已完成")
	}()

	// 返回成功响应
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "主节点间同步已触发，正在后台执行",
	})
}

// 向所有子节点发送区块链同步请求（ai辅助开发@deepseek）
func sendBlockchainSyncToAllNodes(batchIDs []string) {
	p2pMutex.RLock()
	defer p2pMutex.RUnlock()

	for _, node := range knownNodes {
		// 跳过主节点和当前节点
		if node.IsMainNode || node.ID == thisNodeID {
			continue
		}

		// 确保节点地址包含有效的端口号
		nodeAddress := node.Address
		if !strings.Contains(nodeAddress, ":") || strings.HasSuffix(nodeAddress, ":0") || strings.HasSuffix(nodeAddress, ":") {
			// 如果地址没有端口号或端口号为0，则跳过该节点
			log.Printf("跳过无效地址的节点 %s: %s", node.ID, nodeAddress)
			continue
		}

		// 添加重试机制，最多尝试3次连接
		var conn net.Conn
		var err error
		maxRetries := 3
		retryDelay := time.Second

		for attempt := 1; attempt <= maxRetries; attempt++ {
			// 增加连接超时时间到10秒
			conn, err = net.DialTimeout("tcp", nodeAddress, 10*time.Second)
			if err == nil {
				break
			}

			log.Printf("第 %d 次尝试连接子节点 %s 失败: %v", attempt, node.ID, err)
			if attempt < maxRetries {
				time.Sleep(retryDelay)
				retryDelay *= 2 // 指数退避
			}
		}

		if err != nil {
			log.Printf("连接子节点失败 %s: %v", node.ID, err)
			// 标记节点为不可用
			markNodeAsUnavailable(nodeAddress, false)
			continue
		}

		// 设置写入超时
		conn.SetWriteDeadline(time.Now().Add(30 * time.Second))

		// 对每个批次发送同步请求
		for _, batchID := range batchIDs {
			// 创建同步请求消息
			syncRequest := map[string]interface{}{
				"type":           "blockchain_sync_request",
				"batch_id":       batchID,
				"start_index":    0, // 请求所有区块
				"is_pruned_sync": true,
			}

			// 发送同步请求
			e := json.NewEncoder(conn)
			if err := e.Encode(syncRequest); err != nil {
				log.Printf("向节点 %s 发送同步请求失败: %v", node.ID, err)
				conn.Close()
				// 标记节点为不可用
				markNodeAsUnavailable(node.Address, false)
				break
			}

			// 短暂延迟避免请求过于频繁
			time.Sleep(50 * time.Millisecond)
		}

		conn.Close()
	}
}

// 向所有子节点发送轻量级区块链同步请求
func sendBlockchainLightSyncToAllNodes() {
	p2pMutex.RLock()
	defer p2pMutex.RUnlock()

	// 获取区块链概览信息
	overview := getBlockchainOverview()

	for _, node := range knownNodes {
		// 跳过主节点和当前节点
		if node.IsMainNode || node.ID == thisNodeID {
			continue
		}

		// 确保节点地址包含有效的端口号
		nodeAddress := node.Address
		if !strings.Contains(nodeAddress, ":") || strings.HasSuffix(nodeAddress, ":0") || strings.HasSuffix(nodeAddress, ":") {
			// 如果地址没有端口号或端口号为0，则跳过该节点
			log.Printf("跳过无效地址的节点 %s: %s", node.ID, nodeAddress)
			continue
		}

		// 添加重试机制，最多尝试3次连接
		var conn net.Conn
		var err error
		maxRetries := 3
		retryDelay := time.Second

		for attempt := 1; attempt <= maxRetries; attempt++ {
			// 增加连接超时时间到10秒
			conn, err = net.DialTimeout("tcp", nodeAddress, 10*time.Second)
			if err == nil {
				break
			}

			log.Printf("第 %d 次尝试连接子节点 %s 失败: %v", attempt, node.ID, err)
			if attempt < maxRetries {
				time.Sleep(retryDelay)
				retryDelay *= 2 // 指数退避
			}
		}

		if err != nil {
			log.Printf("连接子节点失败 %s: %v", node.ID, err)
			// 标记节点为不可用
			markNodeAsUnavailable(nodeAddress, false)
			continue
		}

		// 设置写入超时
		conn.SetWriteDeadline(time.Now().Add(30 * time.Second))

		// 创建轻量级同步请求消息
		lightSyncRequest := map[string]interface{}{
			"type":                "blockchain_light_sync_request",
			"node_id":             thisNodeID,
			"is_pruned_sync":      false,
			"blockchain_overview": overview,
		}

		// 发送同步请求
		e := json.NewEncoder(conn)
		if err := e.Encode(lightSyncRequest); err != nil {
			log.Printf("向节点 %s 发送轻量级同步请求失败: %v", node.ID, err)
			conn.Close()
			// 标记节点为不可用
			markNodeAsUnavailable(node.Address, false)
			continue
		}

		conn.Close()
	}
}

// 从YOLO服务下载检测结果图片或直接读取本地文件
func downloadDetectionImage(batchID string, level int, imagePath string) error {
	// 检测功能已移除，不再下载和保存检测图片
	log.Printf("检测功能已移除，跳过下载检测图片: batchID=%s, level=%d", batchID, level)
	return nil
}

// ------------------------------- P2P网络相关函数 --------------------------------

// 初始化非对称加密
func initEncryption() error {
	// 尝试从文件加载密钥对
	keyFilePath := filepath.Join(dataDir, "node_key.pem")
	privateKeyBytes, err := os.ReadFile(keyFilePath)
	if err == nil {
		// 从PEM文件解析私钥
		privateKey, err = parsePrivateKey(privateKeyBytes)
		if err == nil {
			log.Println("成功加载现有的RSA密钥对")
			return nil
		}
		log.Printf("加载现有密钥失败，将生成新密钥: %v", err)
	}

	// 生成新的RSA密钥对
	log.Println("生成新的RSA密钥对...")
	privateKey, err = rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return fmt.Errorf("生成RSA密钥对失败: %v", err)
	}

	// 验证生成的密钥
	err = privateKey.Validate()
	if err != nil {
		return fmt.Errorf("验证RSA密钥失败: %v", err)
	}

	// 将私钥保存到文件
	privateKeyBytes = encodePrivateKeyToPEM(privateKey)
	err = os.WriteFile(keyFilePath, privateKeyBytes, 0600) // 只有拥有者可读写
	if err != nil {
		log.Printf("保存私钥失败: %v", err) // 继续执行，但记录警告
	}

	log.Println("RSA密钥对生成成功")
	return nil
}

// 解析PEM格式的私钥
func parsePrivateKey(pemData []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemData)
	if block == nil || block.Type != "RSA PRIVATE KEY" {
		return nil, fmt.Errorf("不是有效的RSA私钥PEM数据")
	}

	// PKCS#1格式的RSA私钥
	privateKey, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err == nil {
		return privateKey, nil
	}

	// 尝试PKCS#8格式
	parsedKey, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("解析私钥失败: %v", err)
	}

	privateKey, ok := parsedKey.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("解析的密钥不是RSA私钥")
	}

	return privateKey, nil
}

// 将私钥编码为PEM格式
func encodePrivateKeyToPEM(privateKey *rsa.PrivateKey) []byte {
	privateKeyBytes := x509.MarshalPKCS1PrivateKey(privateKey)
	privateKeyPEM := pem.EncodeToMemory(
		&pem.Block{
			Type:  "RSA PRIVATE KEY",
			Bytes: privateKeyBytes,
		},
	)
	return privateKeyPEM
}

// 将公钥编码为PEM格式
func encodePublicKeyToPEM(publicKey *rsa.PublicKey) ([]byte, error) {
	publicKeyBytes, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		return nil, err
	}

	publicKeyPEM := pem.EncodeToMemory(
		&pem.Block{
			Type:  "RSA PUBLIC KEY",
			Bytes: publicKeyBytes,
		},
	)
	return publicKeyPEM, nil
}

// 解析PEM格式的公钥
func parsePublicKey(pemData []byte) (*rsa.PublicKey, error) {
	block, _ := pem.Decode(pemData)
	if block == nil || block.Type != "RSA PUBLIC KEY" {
		return nil, fmt.Errorf("不是有效的RSA公钥PEM数据")
	}

	parsedKey, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("解析公钥失败: %v", err)
	}

	publicKey, ok := parsedKey.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("解析的密钥不是RSA公钥")
	}

	return publicKey, nil
}

// 加密消息 - 使用目标节点的公钥
func encryptMessage(nodeID string, plaintext []byte) ([]byte, error) {
	// 获取目标节点的公钥
	publicKey, exists := publicKeyMap[nodeID]
	if !exists {
		return nil, fmt.Errorf("未找到节点 %s 的公钥", nodeID)
	}

	// 使用RSA-OAEP加密算法
	ciphertext, err := rsa.EncryptOAEP(
		sha256.New(),
		rand.Reader,
		publicKey,
		plaintext,
		nil, // 无额外数据
	)
	if err != nil {
		return nil, fmt.Errorf("加密失败: %v", err)
	}

	return ciphertext, nil
}

// 解密消息 - 使用当前节点的私钥
func decryptMessage(ciphertext []byte) ([]byte, error) {
	if privateKey == nil {
		return nil, fmt.Errorf("当前节点没有可用的私钥")
	}

	// 使用RSA-OAEP解密算法
	plaintext, err := rsa.DecryptOAEP(
		sha256.New(),
		rand.Reader,
		privateKey,
		ciphertext,
		nil, // 无额外数据
	)
	if err != nil {
		return nil, fmt.Errorf("解密失败: %v", err)
	}

	return plaintext, nil
}

// 交换公钥 - 在节点握手时调用
func exchangePublicKey(conn net.Conn, remoteNodeID string) error {
	// 发送当前节点的公钥（Base64编码）
	publicKeyPEM, err := encodePublicKeyToPEM(&privateKey.PublicKey)
	if err != nil {
		return fmt.Errorf("编码公钥失败: %v", err)
	}

	publicKeyStr := base64.StdEncoding.EncodeToString(publicKeyPEM)

	// 创建公钥交换消息
	keyExchangeMsg := map[string]interface{}{
		"type":       "key_exchange",
		"node_id":    thisNodeID,
		"public_key": publicKeyStr,
	}

	// 发送公钥交换消息
	e := json.NewEncoder(conn)
	if err := e.Encode(keyExchangeMsg); err != nil {
		return fmt.Errorf("发送公钥失败: %v", err)
	}

	// 等待接收对方的公钥
	d := json.NewDecoder(conn)
	var remoteMsg map[string]interface{}
	if err := d.Decode(&remoteMsg); err != nil {
		return fmt.Errorf("接收远程公钥失败: %v", err)
	}

	// 解析接收到的公钥
	if msgType, ok := remoteMsg["type"].(string); ok && msgType == "key_exchange" {
		if remotePublicKeyStr, ok := remoteMsg["public_key"].(string); ok {
			remotePublicKeyPEM, err := base64.StdEncoding.DecodeString(remotePublicKeyStr)
			if err != nil {
				return fmt.Errorf("解码远程公钥失败: %v", err)
			}

			remotePublicKey, err := parsePublicKey(remotePublicKeyPEM)
			if err != nil {
				return fmt.Errorf("解析远程公钥失败: %v", err)
			}

			// 保存远程节点的公钥
			publicKeyMap[remoteNodeID] = remotePublicKey
			log.Printf("成功交换公钥 - 节点: %s", remoteNodeID)
			return nil
		}
	}

	return fmt.Errorf("收到的消息不是有效的公钥交换消息")
}

// 获取节点的公钥 - 如果不存在则尝试交换
func getPublicKeyFromNode(nodeAddress string) (*rsa.PublicKey, error) {
	// 查找节点信息
	var node NodeInfo
	p2pMutex.RLock()
	for _, n := range knownNodes {
		if n.Address == nodeAddress {
			node = n
			break
		}
	}
	p2pMutex.RUnlock()

	if node.ID == "" {
		return nil, fmt.Errorf("未找到节点: %s", nodeAddress)
	}

	// 检查是否已经有该节点的公钥
	if publicKey, exists := publicKeyMap[node.ID]; exists {
		return publicKey, nil
	}

	// 如果没有公钥，尝试临时连接获取
	conn, err := net.DialTimeout("tcp", nodeAddress, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("连接节点失败: %v", err)
	}
	defer conn.Close()

	// 进行公钥交换
	if err := exchangePublicKey(conn, node.ID); err != nil {
		return nil, fmt.Errorf("交换公钥失败: %v", err)
	}

	// 返回获取到的公钥
	return publicKeyMap[node.ID], nil
}

// 发送加密消息 - 尝试使用目标节点的公钥加密消息
func sendEncryptedMessage(conn net.Conn, nodeID string, message map[string]interface{}) error {
	// 检查是否有目标节点的公钥
	publicKey, exists := publicKeyMap[nodeID]
	if exists {
		log.Printf("使用公钥加密消息发送到节点: %s", nodeID)

		// 将消息序列化为JSON
		messageBytes, err := json.Marshal(message)
		if err != nil {
			return fmt.Errorf("序列化消息失败: %v", err)
		}

		// 使用RSA-OAEP加密算法加密消息
		ciphertext, err := rsa.EncryptOAEP(
			sha256.New(),
			rand.Reader,
			publicKey,
			messageBytes,
			nil, // 无额外数据
		)
		if err != nil {
			return fmt.Errorf("加密消息失败: %v", err)
		}

		// 将加密消息编码为Base64，以便在JSON中传输
		encryptedMessage := base64.StdEncoding.EncodeToString(ciphertext)

		// 创建加密消息包装器
		encryptedWrapper := map[string]interface{}{
			"type":      "encrypted_message",
			"sender_id": thisNodeID,
			"message":   encryptedMessage,
		}

		// 发送加密消息
		e := json.NewEncoder(conn)
		if err := e.Encode(encryptedWrapper); err != nil {
			return fmt.Errorf("发送加密消息失败: %v", err)
		}

		log.Printf("成功发送加密消息到节点: %s", nodeID)
		return nil
	} else {
		// 作为后备方案，发送未加密的消息
		log.Printf("警告: 未找到节点 %s 的公钥，发送未加密消息", nodeID)
		e := json.NewEncoder(conn)
		if err := e.Encode(message); err != nil {
			return fmt.Errorf("发送未加密消息失败: %v", err)
		}
		return nil
	}
}

// 初始化P2P网络
func initP2PNetwork() error {
	// 初始化负载信息映射
	loadBalancingInfo = make(map[string]NodeLoadInfo)

	// 优先使用代码中预定义的主节点配置
	// 从环境变量读取主节点配置（如果有设置）
	mainNodeList := os.Getenv("MAIN_NODES")
	if mainNodeList != "" {
		mainNodes = strings.Split(mainNodeList, ",")
	}
	// 环境变量示例: MAIN_NODES=111.230.7.188:3000,152.136.16.231:3000

	// 从环境变量获取节点类型配置，默认为主节点
	envValue := os.Getenv("IS_MAIN_NODE")
	isMainNode = envValue != "false" // 默认为true，除非明确设置为false

	// 生成节点ID
	thisNodeID = generateNodeID()
	localIP := getLocalIP()
	localAddress := fmt.Sprintf("%s:%d", localIP, p2pPort)

	// 初始化节点列表
	knownNodes = []NodeInfo{
		{ID: thisNodeID, Address: localAddress, IsMainNode: isMainNode, LastSeen: time.Now().Unix()},
	}

	// 如果配置了其他主节点，添加到已知节点列表
	for _, nodeAddr := range mainNodes {
		if nodeAddr != "" && nodeAddr != localAddress {
			// 确保主节点地址包含有效的端口号
			if !strings.Contains(nodeAddr, ":") || strings.HasSuffix(nodeAddr, ":0") || strings.HasSuffix(nodeAddr, ":") {
				// 如果地址没有端口号或端口号为0，则使用默认P2P端口
				if strings.Contains(nodeAddr, ":") {
					// 移除无效的端口号
					nodeAddr = strings.Split(nodeAddr, ":")[0]
				}
				nodeAddr = fmt.Sprintf("%s:%d", nodeAddr, p2pPort)
				log.Printf("修正主节点地址为: %s", nodeAddr)
			}

			// 为主节点创建临时ID，实际连接后会更新
			hash := sha256.Sum256([]byte(nodeAddr))
			tempNodeID := fmt.Sprintf("node_%x", hash[:8])
			knownNodes = append(knownNodes, NodeInfo{
				ID:         tempNodeID,
				Address:    nodeAddr,
				IsMainNode: true,
				LastSeen:   time.Now().Unix(),
			})
		}
	}

	// 记录首选主节点
	if len(mainNodes) > 0 {
		currentMainNode = mainNodes[0]
	}

	log.Printf("P2P网络初始化 - 节点ID: %s, 端口: %d, 是主节点: %v", thisNodeID, p2pPort, isMainNode)
	if len(mainNodes) > 0 {
		log.Printf("配置的主节点列表: %v", mainNodes)
	}

	// 启动节点自动全同步协程
	go startNodeAutoSync()
	return nil
}

// 生成节点ID
func generateNodeID() string {
	h := sha256.New()
	h.Write([]byte(fmt.Sprintf("%s:%d:%d", getLocalIP(), p2pPort, time.Now().UnixNano())))
	return fmt.Sprintf("node_%x", h.Sum(nil)[:8])
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

// 启动P2P服务器
func startP2PServer() error {
	var err error
	p2pServer, err = net.Listen("tcp", fmt.Sprintf(":%d", p2pPort))
	if err != nil {
		return fmt.Errorf("无法启动P2P服务器: %v", err)
	}

	log.Printf("P2P服务器启动成功，监听端口: %d", p2pPort)

	// 启动节点清理协程
	go cleanupInactiveNodes()

	// 接受连接
	go func() {
		for {
			conn, err := p2pServer.Accept()
			if err != nil {
				// 检查是否是因为服务器关闭导致的错误
				if strings.Contains(err.Error(), "use of closed network connection") {
					break
				}
				log.Printf("接受P2P连接失败: %v", err)
				continue
			}
			// 处理连接
			go handleP2PConnection(conn)
		}
	}()

	return nil
}

// 处理P2P连接
func handleP2PConnection(conn net.Conn) {
	defer conn.Close()

	// 设置连接超时
	conn.SetDeadline(time.Now().Add(5 * time.Minute))

	// 检查连接来源是否在允许的主节点列表中
	remoteAddr := conn.RemoteAddr().String()
	isAllowedNode := false

	// 检查是否是允许的主节点地址
	for _, allowedNode := range mainNodes {
		// 提取IP地址部分进行比较（忽略端口）
		allowedIP := strings.Split(allowedNode, ":")[0]
		remoteIP := strings.Split(remoteAddr, ":")[0]

		if remoteIP == allowedIP {
			isAllowedNode = true
			break
		}
	}

	// 创建一个解码器用于读取JSON消息
	d := json.NewDecoder(conn)

	// 启动心跳协程
	heartbeatTicker := time.NewTicker(30 * time.Second)
	defer heartbeatTicker.Stop()

	go func() {
		for range heartbeatTicker.C {
			// 发送心跳消息
			heartbeatMsg := map[string]interface{}{
				"type": "heartbeat",
			}
			conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			e := json.NewEncoder(conn)
			if err := e.Encode(heartbeatMsg); err != nil {
				log.Printf("发送心跳消息失败: %v", err)
				return
			}
		}
	}()

	// 持续处理消息
	for {
		// 更新读取超时
		conn.SetReadDeadline(time.Now().Add(2 * time.Minute))

		// 读取消息类型
		var msg map[string]interface{}
		if err := d.Decode(&msg); err != nil {
			// 优雅处理连接错误
			if netErr, ok := err.(net.Error); ok {
				// 处理连接重置错误 - 这是常见的网络问题，不需要详细日志
				if strings.Contains(netErr.Error(), "connection reset by peer") {
					log.Printf("P2P连接被远程节点重置: %s", remoteAddr)
				} else if netErr.Timeout() {
					log.Printf("P2P连接读取超时: %s", remoteAddr)
				} else {
					//log.Printf("解析P2P消息失败: %v", err)
				}
			} else if err != io.EOF {
				//log.Printf("解析P2P消息失败: %v", err)
			}
			return
		}

		// 获取消息类型
		msgType, ok := msg["type"].(string)
		if !ok {
			log.Printf("无效的P2P消息: 缺少type字段")
			return
		}

		// 如果是心跳消息，记录并继续
		if msgType == "heartbeat" {
			log.Printf("收到心跳消息 from %s", remoteAddr)
			continue
		}

		// 处理加密消息
		if msgType == "encrypted_message" {
			// 安全地提取发送者ID
			senderID, senderIDOk := msg["sender_id"].(string)
			encryptedMsg, encryptedMsgOk := msg["message"].(string)

			if !senderIDOk || !encryptedMsgOk {
				log.Printf("无效的加密消息: 缺少必要字段")
				return
			}

			log.Printf("收到来自节点 %s 的加密消息", senderID)

			// 解码Base64加密消息
			ciphertext, err := base64.StdEncoding.DecodeString(encryptedMsg)
			if err != nil {
				log.Printf("解码加密消息失败: %v", err)
				return
			}

			// 使用当前节点的私钥解密消息
			plaintext, err := decryptMessage(ciphertext)
			if err != nil {
				log.Printf("解密消息失败: %v", err)
				return
			}

			// 解析解密后的JSON消息
			var decryptedMsg map[string]interface{}
			if err := json.Unmarshal(plaintext, &decryptedMsg); err != nil {
				log.Printf("解析解密后的消息失败: %v", err)
				return
			}

			// 验证解密后消息的有效性
			decryptedMsgType, decryptedMsgTypeOk := decryptedMsg["type"].(string)
			if !decryptedMsgTypeOk {
				log.Printf("解密后的消息无效: 缺少type字段")
				return
			}

			log.Printf("成功解密消息，原始消息类型: %s", decryptedMsgType)

			// 使用解密后的消息替换原始消息
			msg = decryptedMsg
			msgType = decryptedMsgType
		}

		// 检查节点是否已在已知节点列表中
		isKnownNode := false
		var nodeID string
		if nodeIDInterface, exists := msg["node_id"]; exists {
			nodeID, _ = nodeIDInterface.(string)
			p2pMutex.RLock()
			for _, node := range knownNodes {
				if node.ID == nodeID {
					isKnownNode = true
					break
				}
			}
			p2pMutex.RUnlock()
		}

		// 如果当前节点是主节点，则只允许来自指定主节点地址的连接访问需要授权的同步功能
		// 但对于已知节点，我们允许它们进行同步操作
		if isMainNode && !isAllowedNode && !isKnownNode && !strings.HasPrefix(remoteAddr, "127.0.0.1:") && !strings.HasPrefix(remoteAddr, "[::1]:") {
			// 允许节点握手、轻节点同步和剪枝节点同步请求
			if msgType != "node_handshake" && msgType != "blockchain_sync_request" && msgType != "blockchain_overview_request" {
				log.Printf("拒绝来自未授权地址 %s 的主节点同步访问，消息类型: %s", remoteAddr, msgType)
				return
			}
		}

		// 根据消息类型处理
		switch msgType {
		case "node_handshake":
			handleNodeHandshake(conn, msg)
		case "blockchain_sync_request":
			// 允许所有节点进行轻量级同步请求（剪枝同步）
			handleBlockchainSyncRequest(conn, msg)
		case "blockchain_light_sync_request":
			// 允许所有节点进行轻量级同步请求
			handleBlockchainLightSyncRequest(conn, msg)
		case "new_block":
			handleNewBlockMessage(conn, msg)
		case "blockchain_full_sync_request":
			// 检查是否允许该节点访问全量同步功能
			if isMainNode && !isAllowedNode && !isKnownNode && !strings.HasPrefix(remoteAddr, "127.0.0.1:") && !strings.HasPrefix(remoteAddr, "[::1]:") {
				log.Printf("拒绝来自未授权地址 %s 的区块链全量同步请求", remoteAddr)
				return
			}
			handleBlockchainFullSyncRequest(conn, msg)
		case "blockchain_overview_request":
			// 允许所有节点进行概览请求（剪枝节点同步）
			handleBlockchainOverviewRequest(conn, msg)
		case "blockchain_full_batch_request":
			// 检查是否允许该节点访问批次数据功能
			if isMainNode && !isAllowedNode && !isKnownNode && !strings.HasPrefix(remoteAddr, "127.0.0.1:") && !strings.HasPrefix(remoteAddr, "[::1]:") {
				log.Printf("拒绝来自未授权地址 %s 的区块链批次数据请求", remoteAddr)
				return
			}
			handleBlockchainFullBatchRequest(conn, msg)
		case "blockchain_sync_response":
			// 处理区块链同步响应
			handleBlockchainSyncResponse(conn, msg)
		case "blockchain_full_batch_data":
			// 处理区块链全量批次数据响应
			handleBlockchainSyncResponse(conn, msg)
		default:
			log.Printf("未知的P2P消息类型: %s", msgType)
		}
	}
}

// 处理节点握手
func handleNodeHandshake(conn net.Conn, msg map[string]interface{}) {
	// 安全地提取节点信息
	var nodeID, address string
	var isMainNode bool

	if nodeIDVal, ok := msg["node_id"].(string); ok {
		nodeID = nodeIDVal
	}

	if addressVal, ok := msg["address"].(string); ok {
		address = addressVal
	}

	if isMainNodeVal, ok := msg["is_main_node"].(bool); ok {
		isMainNode = isMainNodeVal
	}

	// 获取远程地址
	// 由于 remoteAddr 声明后未使用，移除该声明

	if nodeID == "" || address == "" {
		log.Printf("无效的节点握手消息: 缺少必要字段")
		return
	}

	// 确保地址包含有效的端口号
	originalAddress := address
	if !strings.Contains(address, ":") || strings.HasSuffix(address, ":0") || strings.HasSuffix(address, ":") {
		// 如果地址没有端口号或端口号为0，则使用默认P2P端口
		if strings.Contains(address, ":") {
			// 移除无效的端口号
			address = strings.Split(address, ":")[0]
		}
		address = fmt.Sprintf("%s:%d", address, p2pPort)

		// 只有在地址确实被修正时才记录日志
		if originalAddress != address {
			log.Printf("修正节点地址为: %s", address)
		}
	}

	// 更新节点列表
	p2pMutex.Lock()
	nodeExists := false
	for i, node := range knownNodes {
		if node.ID == nodeID {
			nodeExists = true
			knownNodes[i].LastSeen = time.Now().Unix()
			knownNodes[i].Address = address
			knownNodes[i].IsMainNode = isMainNode
			break
		}
	}

	if !nodeExists {
		newNode := NodeInfo{
			ID:         nodeID,
			Address:    address,
			IsMainNode: isMainNode,
			LastSeen:   time.Now().Unix(),
		}
		knownNodes = append(knownNodes, newNode)
		log.Printf("新节点加入P2P网络: %s (%s)", nodeID, address)
	} else {
		// 只有在地址确实发生变化时才记录更新日志
		// 查找节点以获取旧地址
		var oldAddress string
		for _, node := range knownNodes {
			if node.ID == nodeID {
				oldAddress = node.Address
				break
			}
		}

		if oldAddress != address {
			log.Printf("更新已知节点信息: %s (%s -> %s)", nodeID, oldAddress, address)
		}
	}
	p2pMutex.Unlock()

	// 执行公钥交换
	log.Printf("开始与节点 %s 的公钥交换", nodeID)
	if err := exchangePublicKey(conn, nodeID); err != nil {
		//log.Printf("公钥交换失败: %v", err)
		// 公钥交换失败，仍然返回握手响应，但后续通信可能不会加密
	} else {
		log.Printf("成功完成与节点 %s 的公钥交换", nodeID)
	}

	// 返回握手响应，包含当前节点信息和区块链概览
	response := map[string]interface{}{
		"type":                "handshake_response",
		"node_id":             thisNodeID,
		"is_main_node":        true,
		"blockchain_overview": getBlockchainOverview(),
	}

	// 发送响应
	e := json.NewEncoder(conn)
	if err := e.Encode(response); err != nil {
		log.Printf("发送区块链同步响应失败: %v", err)
		return
	}
}

// 获取区块链概览信息
func getBlockchainOverview() []map[string]interface{} {
	overview := []map[string]interface{}{}

	p2pMutex.RLock()
	defer p2pMutex.RUnlock()

	syncMutex.RLock()
	defer syncMutex.RUnlock()

	for batchID, chain := range blockchains {
		if len(chain.Chain) > 0 {
			lastBlock := chain.Chain[len(chain.Chain)-1]
			overview = append(overview, map[string]interface{}{
				"batch_id":         batchID,
				"length":           len(chain.Chain),
				"last_block_index": lastBlock.Index,
				"last_block_hash":  lastBlock.CurrentHash,
				"last_timestamp":   lastBlock.Timestamp,
			})
		}
	}
	return overview
}

// 处理区块链同步请求
func handleBlockchainSyncRequest(conn net.Conn, msg map[string]interface{}) {
	// 安全地提取请求的批次ID
	var batchID string
	if batchIDVal, ok := msg["batch_id"].(string); ok {
		batchID = batchIDVal
	}

	// 安全地提取起始区块索引，如果不存在则默认为0
	var startIndex int
	if startIndexVal, ok := msg["start_index"]; ok {
		if startIndexFloat, ok := startIndexVal.(float64); ok {
			startIndex = int(startIndexFloat)
		}
	}

	// 检查是否存在该批次的区块链
	syncMutex.RLock()
	chain, exists := blockchains[batchID]
	syncMutex.RUnlock()

	if !exists || len(chain.Chain) == 0 {
		// 返回空响应
		response := map[string]interface{}{
			"type":    "blockchain_sync_response",
			"success": false,
			"message": "批次不存在",
		}
		e := json.NewEncoder(conn)
		if err := e.Encode(response); err != nil {
			log.Printf("发送区块链轻量级同步响应失败: %v", err)
			return
		}
		return
	}

	// 准备要发送的区块数据（只发送区块头和必要信息）
	blocksToSend := []map[string]interface{}{}
	for i := startIndex; i < len(chain.Chain); i++ {
		block := chain.Chain[i]
		// 构建轻量级区块数据（不含完整的ProductInfo）
		lightBlock := map[string]interface{}{
			"index":         block.Index,
			"timestamp":     block.Timestamp,
			"level":         block.Level,
			"previous_hash": block.PreviousHash,
			"current_hash":  block.CurrentHash,
			"merkle_root":   block.MerkleRoot,
			// 仅包含必要的产品信息
			"product_info": map[string]interface{}{
				"batch_id":       block.ProductInfo.BatchID,
				"processor_id":   block.ProductInfo.ProcessorID,
				"image_hash":     block.ProductInfo.ImageHash,
				"detection_hash": block.ProductInfo.DetectionHash,
			},
		}
		blocksToSend = append(blocksToSend, lightBlock)
	}

	// 返回同步响应
	response := map[string]interface{}{
		"type":         "blockchain_sync_response",
		"success":      true,
		"batch_id":     batchID,
		"blocks":       blocksToSend,
		"total_blocks": len(chain.Chain),
	}

	e := json.NewEncoder(conn)
	if err := e.Encode(response); err != nil {
		log.Printf("发送区块链全量同步响应失败: %v", err)
		return
	}
}

// 处理区块链轻量级同步请求
func handleBlockchainLightSyncRequest(conn net.Conn, msg map[string]interface{}) {
	// 安全地提取请求的批次ID
	var batchID string
	if batchIDVal, ok := msg["batch_id"].(string); ok {
		batchID = batchIDVal
	}

	// 安全地提取起始区块索引，如果不存在则默认为0
	var startIndex int
	if startIndexVal, ok := msg["start_index"]; ok {
		if startIndexFloat, ok := startIndexVal.(float64); ok {
			startIndex = int(startIndexFloat)
		}
	}

	// 检查是否存在该批次的区块链
	syncMutex.RLock()
	chain, exists := blockchains[batchID]
	syncMutex.RUnlock()

	if !exists || len(chain.Chain) == 0 {
		// 返回空响应
		response := map[string]interface{}{
			"type":    "blockchain_sync_response",
			"success": false,
			"message": "批次不存在",
		}
		e := json.NewEncoder(conn)
		if err := e.Encode(response); err != nil {
			log.Printf("发送区块链轻量级同步响应失败: %v", err)
			return
		}
		return
	}

	// 准备要发送的区块数据（只发送区块头和必要信息）
	blocksToSend := []map[string]interface{}{}
	for i := startIndex; i < len(chain.Chain); i++ {
		block := chain.Chain[i]
		// 构建轻量级区块数据（不含完整的ProductInfo）
		lightBlock := map[string]interface{}{
			"index":         block.Index,
			"timestamp":     block.Timestamp,
			"level":         block.Level,
			"previous_hash": block.PreviousHash,
			"current_hash":  block.CurrentHash,
			"merkle_root":   block.MerkleRoot,
			// 仅包含必要的产品信息
			"product_info": map[string]interface{}{
				"batch_id":       block.ProductInfo.BatchID,
				"processor_id":   block.ProductInfo.ProcessorID,
				"image_hash":     block.ProductInfo.ImageHash,
				"detection_hash": block.ProductInfo.DetectionHash,
			},
		}
		blocksToSend = append(blocksToSend, lightBlock)
	}

	// 返回同步响应
	response := map[string]interface{}{
		"type":         "blockchain_sync_response",
		"success":      true,
		"batch_id":     batchID,
		"blocks":       blocksToSend,
		"total_blocks": len(chain.Chain),
	}

	e := json.NewEncoder(conn)
	if err := e.Encode(response); err != nil {
		log.Printf("发送完整批次数据响应失败: %v", err)
		return
	}
}

// 处理区块链全量同步请求
func handleBlockchainFullSyncRequest(conn net.Conn, msg map[string]interface{}) {
	// 安全地提取请求的批次ID和请求节点ID
	var batchID, requesterNodeID string
	if batchIDVal, ok := msg["batch_id"].(string); ok {
		batchID = batchIDVal
	}
	if requesterNodeIDVal, ok := msg["node_id"].(string); ok {
		requesterNodeID = requesterNodeIDVal
	}

	log.Printf("收到来自节点 %s 的全量同步请求，批次ID: %s", requesterNodeID, batchID)

	// 检查是否存在该批次的区块链
	syncMutex.RLock()
	chain, exists := blockchains[batchID]
	syncMutex.RUnlock()

	if !exists || len(chain.Chain) == 0 {
		response := map[string]interface{}{
			"type":    "blockchain_full_sync_response",
			"success": false,
			"message": "批次不存在",
		}
		e := json.NewEncoder(conn)
		if err := e.Encode(response); err != nil {
			log.Printf("发送区块链同步响应失败: %v", err)
			return
		}
		return
	}

	// 准备完整的区块数据
	blocksToSend := []map[string]interface{}{}
	for _, block := range chain.Chain {
		fullBlock := map[string]interface{}{
			"index":         block.Index,
			"timestamp":     block.Timestamp,
			"level":         block.Level,
			"previous_hash": block.PreviousHash,
			"current_hash":  block.CurrentHash,
			"merkle_root":   block.MerkleRoot,
			// 包含完整的产品信息
			"product_info": map[string]interface{}{
				"batch_id":       block.ProductInfo.BatchID,
				"processor_id":   block.ProductInfo.ProcessorID,
				"process_date":   block.ProductInfo.ProcessDate,
				"details":        block.ProductInfo.Details,
				"note":           block.ProductInfo.Note,
				"image_hash":     block.ProductInfo.ImageHash,
				"detection_hash": block.ProductInfo.DetectionHash,
				"location":       block.ProductInfo.Location,
				"quality_check":  block.ProductInfo.QualityCheck,
			},
		}
		blocksToSend = append(blocksToSend, fullBlock)
	}

	// 返回全量同步响应
	response := map[string]interface{}{
		"type":         "blockchain_full_batch_data",
		"success":      true,
		"batch_id":     batchID,
		"blocks":       blocksToSend,
		"total_blocks": len(chain.Chain),
	}

	e := json.NewEncoder(conn)
	if err := e.Encode(response); err != nil {
		log.Printf("发送区块链概览响应失败: %v", err)
		return
	}
}

// 处理区块链同步响应
func handleBlockchainSyncResponse(conn net.Conn, msg map[string]interface{}) {
	// 获取消息类型
	msgType, typeOk := msg["type"].(string)

	// 安全地提取成功标志
	var success bool
	if successVal, ok := msg["success"].(bool); ok {
		success = successVal
	} else if typeOk && msgType == "blockchain_full_batch_data" {
		// 对于blockchain_full_batch_data类型的消息，如果有blocks字段，则视为成功
		if _, hasBlocks := msg["blocks"]; hasBlocks {
			success = true
		}
	}

	// 安全地提取消息
	var message string
	if messageVal, ok := msg["message"].(string); ok {
		message = messageVal
	}

	// 安全地提取批次ID
	var batchID string
	if batchIDVal, ok := msg["batchID"].(string); ok {
		batchID = batchIDVal
	} else if batchIDVal, ok := msg["batch_id"].(string); ok {
		batchID = batchIDVal
	}

	if !success {
		if message != "" {
			log.Printf("区块链同步失败: %s", message)
		} else if typeOk {
			log.Printf("区块链同步失败: 类型=%s, 未知错误", msgType)
		} else {
			log.Printf("区块链同步失败: 未知错误")
		}
		return
	}

	// 安全地提取区块数据
	var blocksData []interface{}
	if blocksDataVal, ok := msg["blocks"].([]interface{}); ok {
		blocksData = blocksDataVal
	}

	if len(blocksData) == 0 {
		log.Printf("区块链同步完成，但未收到区块数据")
		return
	}

	var blocks []Block
	for _, blockData := range blocksData {
		blockMap, ok := blockData.(map[string]interface{})
		if !ok {
			log.Printf("无效的区块数据格式")
			continue
		}
		block := Block{
			Index:        0,
			Timestamp:    0,
			Level:        0,
			PreviousHash: "",
			CurrentHash:  "",
		}

		// 安全地提取区块索引
		if indexVal, ok := blockMap["index"]; ok {
			if indexFloat, ok := indexVal.(float64); ok {
				block.Index = int(indexFloat)
			}
		}

		// 安全地提取时间戳
		if timestampVal, ok := blockMap["timestamp"]; ok {
			if timestampFloat, ok := timestampVal.(float64); ok {
				block.Timestamp = int64(timestampFloat)
			}
		}

		// 安全地提取级别
		if levelVal, ok := blockMap["level"]; ok {
			if levelFloat, ok := levelVal.(float64); ok {
				block.Level = int(levelFloat)
			}
		}

		// 安全地提取哈希值
		if previousHash, ok := blockMap["previous_hash"].(string); ok {
			block.PreviousHash = previousHash
		}

		if currentHash, ok := blockMap["current_hash"].(string); ok {
			block.CurrentHash = currentHash
		}

		// 安全地提取Merkle根
		if merkleRoot, ok := blockMap["merkle_root"].(string); ok {
			block.MerkleRoot = merkleRoot
		}

		// 解析产品信息
		block.ProductInfo = ProductInfo{
			BatchID:       "",
			ProcessorID:   "",
			ProcessDate:   "",
			Details:       "",
			Note:          "",
			ImageHash:     "",
			DetectionHash: "",
			Location:      "",
			QualityCheck:  "",
		}

		// 安全地提取产品信息
		if productInfoMap, ok := blockMap["product_info"].(map[string]interface{}); ok {
			// 优先尝试提取batchID，如果不存在则尝试batch_id（兼容旧版本）
			if batchID, ok := productInfoMap["batchID"].(string); ok {
				block.ProductInfo.BatchID = batchID
			} else if batchID, ok := productInfoMap["batch_id"].(string); ok {
				block.ProductInfo.BatchID = batchID
			}
			if processorID, ok := productInfoMap["processor_id"].(string); ok {
				block.ProductInfo.ProcessorID = processorID
			}
			if processDate, ok := productInfoMap["process_date"].(string); ok {
				block.ProductInfo.ProcessDate = processDate
			}
			if details, ok := productInfoMap["details"].(string); ok {
				block.ProductInfo.Details = details
			}
			if note, ok := productInfoMap["note"].(string); ok {
				block.ProductInfo.Note = note
			}
			if imageHash, ok := productInfoMap["image_hash"].(string); ok {
				block.ProductInfo.ImageHash = imageHash
			}
			if detectionHash, ok := productInfoMap["detection_hash"].(string); ok {
				block.ProductInfo.DetectionHash = detectionHash
			}
			if location, ok := productInfoMap["location"].(string); ok {
				block.ProductInfo.Location = location
			}
			if qualityCheck, ok := productInfoMap["quality_check"].(string); ok {
				block.ProductInfo.QualityCheck = qualityCheck
			}
		}

		blocks = append(blocks, block)
	}

	// 更新本地区块链
	syncMutex.Lock()
	blockchains[batchID] = Blockchain{
		BatchID: batchID,
		Chain:   blocks,
	}

	// 更新批次信息
	if len(blocks) > 0 {
		lastBlock := blocks[len(blocks)-1]
		batchInfos[batchID] = BatchInfo{
			BatchID:      batchID,
			ProductType:  productType,
			CreationDate: time.Unix(lastBlock.Timestamp, 0),
			CurrentLevel: lastBlock.Level,
			CurrentHash:  lastBlock.CurrentHash,
			Complete:     lastBlock.Level >= maxProcessLevel,
		}
	}
	syncMutex.Unlock()

	log.Printf("区块链同步完成，批次: %s, 区块数: %d", batchID, len(blocks))

	// 保存到文件和数据库
	saveBlockchains()
	saveBatchInfoToDB(batchID)
}

// 处理区块链概览请求
func handleBlockchainOverviewRequest(conn net.Conn, msg map[string]interface{}) {
	// 安全地提取请求节点ID
	var requesterNodeID string
	if requesterNodeIDVal, ok := msg["node_id"].(string); ok {
		requesterNodeID = requesterNodeIDVal
	}
	log.Printf("收到来自节点 %s 的区块链概览请求", requesterNodeID)

	// 获取区块链概览信息
	overview := getBlockchainOverview()

	// 返回概览响应
	response := map[string]interface{}{
		"type":                "blockchain_overview_response",
		"success":             true,
		"blockchain_overview": overview,
	}

	e := json.NewEncoder(conn)
	if err := e.Encode(response); err != nil {
		log.Printf("发送新区块消息响应失败: %v", err)
		return
	}
}

// 处理完整批次数据请求
func handleBlockchainFullBatchRequest(conn net.Conn, msg map[string]interface{}) {
	// 安全地提取批次ID
	var batchID string
	if batchIDVal, ok := msg["batch_id"].(string); ok {
		batchID = batchIDVal
	}
	log.Printf("收到完整批次数据请求，批次ID: %s", batchID)

	// 检查是否存在该批次的区块链
	syncMutex.RLock()
	chain, exists := blockchains[batchID]
	syncMutex.RUnlock()

	if !exists || len(chain.Chain) == 0 {
		response := map[string]interface{}{
			"type":    "blockchain_full_batch_data",
			"success": false,
			"message": "批次不存在",
		}
		e := json.NewEncoder(conn)
		if err := e.Encode(response); err != nil {
			log.Printf("发送响应失败: %v", err)
			return
		}
		return
	}

	// 准备完整的区块数据
	blocksToSend := []map[string]interface{}{}
	for _, block := range chain.Chain {
		fullBlock := map[string]interface{}{
			"index":         block.Index,
			"timestamp":     block.Timestamp,
			"level":         block.Level,
			"previous_hash": block.PreviousHash,
			"current_hash":  block.CurrentHash,
			"merkle_root":   block.MerkleRoot,
			// 包含完整的产品信息
			"product_info": map[string]interface{}{
				"batch_id":       block.ProductInfo.BatchID,
				"processor_id":   block.ProductInfo.ProcessorID,
				"process_date":   block.ProductInfo.ProcessDate,
				"details":        block.ProductInfo.Details,
				"note":           block.ProductInfo.Note,
				"image_hash":     block.ProductInfo.ImageHash,
				"detection_hash": block.ProductInfo.DetectionHash,
				"location":       block.ProductInfo.Location,
				"quality_check":  block.ProductInfo.QualityCheck,
			},
		}
		blocksToSend = append(blocksToSend, fullBlock)
	}

	// 返回完整批次数据
	response := map[string]interface{}{
		"type":         "blockchain_full_batch_data",
		"success":      true,
		"batch_id":     batchID,
		"blocks":       blocksToSend,
		"total_blocks": len(chain.Chain),
	}

	e := json.NewEncoder(conn)
	if err := e.Encode(response); err != nil {
		log.Printf("发送响应失败: %v", err)
		return
	}
}

// 处理新区块消息
func handleNewBlockMessage(conn net.Conn, msg map[string]interface{}) {
	// 此函数在主节点上可能不需要实现，因为主节点通常是新区块的创建者
	// 但为了网络稳定性，我们仍然记录收到的新区块
	// 安全地提取批次ID
	var batchID string
	if batchIDVal, ok := msg["batch_id"].(string); ok {
		batchID = batchIDVal
	}

	// 安全地提取区块索引，如果不存在则默认为0
	var blockIndex int
	if blockIndexVal, ok := msg["index"]; ok {
		if blockIndexFloat, ok := blockIndexVal.(float64); ok {
			blockIndex = int(blockIndexFloat)
		}
	}

	log.Printf("收到新区块通知 - 批次: %s, 索引: %d", batchID, blockIndex)
}

// 清理不活跃的节点
func cleanupInactiveNodes() {
	ticker := time.NewTicker(nodeCleanupInterval)
	defer ticker.Stop()

	for range ticker.C {
		currentTime := time.Now().Unix()
		inactiveThreshold := currentTime - 300 // 5分钟不活跃视为离线

		p2pMutex.Lock()
		activeNodes := []NodeInfo{}
		for _, node := range knownNodes {
			// 保留主节点和活跃的子节点
			if node.IsMainNode || node.LastSeen > inactiveThreshold {
				activeNodes = append(activeNodes, node)
			} else {
				log.Printf("移除不活跃节点: %s (%s)", node.ID, node.Address)
			}
		}
		knownNodes = activeNodes
		p2pMutex.Unlock()
	}
}

// ------------------------------- 主函数 --------------------------------
func main() {
	defer db.Close()

	// 初始化非对称加密密钥
	if err := initEncryption(); err != nil {
		log.Fatalf("加密初始化失败: %v", err)
	}

	// 初始化P2P网络
	if err := initP2PNetwork(); err != nil {
		log.Fatalf("P2P网络初始化失败: %v", err)
	}

	// 启动P2P服务器
	if err := startP2PServer(); err != nil {
		log.Fatalf("启动P2P服务器失败: %v", err)
	}

	r := mux.NewRouter()
	r.Use(enableCORS)

	// 公开路由 - 无需认证
	r.HandleFunc("/api/auth/login", loginHandler).Methods("POST", "OPTIONS")
	r.HandleFunc("/api/auth/register", registerUserHandler).Methods("POST", "OPTIONS")
	r.HandleFunc("/product/{batch_id}", getProductChainHandler).Methods("GET", "OPTIONS")
	r.HandleFunc("/health", healthCheckHandler).Methods("GET", "OPTIONS")
	r.HandleFunc("/api/batches", listBatchesHandler).Methods("GET", "OPTIONS")
	// P2P网络信息端点 - 供子节点获取主节点信息
	r.HandleFunc("/api/node/info", getNodeInfoHandler).Methods("GET", "OPTIONS")
	// 负载均衡相关接口
	r.HandleFunc("/api/nodes", getNodeListHandler).Methods("GET", "OPTIONS")
	r.HandleFunc("/api/nodes/best", getBestNodeHandler).Methods("GET", "OPTIONS")

	// 图片服务
	imageHandler := http.StripPrefix("/images/",
		http.FileServer(http.Dir(imagesDir)))
	r.PathPrefix("/images/").Handler(imageHandler)

	// 已移除检测结果图片服务

	// 需要认证的路由
	authRouter := r.PathPrefix("/api").Subrouter()
	authRouter.Use(authenticateMiddleware)
	authRouter.HandleFunc("/auth/me", getCurrentUserHandler).Methods("GET", "OPTIONS")
	authRouter.HandleFunc("/auth/validate", validateTokenHandler).Methods("GET", "OPTIONS")
	authRouter.HandleFunc("/batch/register", registerBatchHandler).Methods("POST", "OPTIONS")
	authRouter.HandleFunc("/process", addProcessingHandler).Methods("POST", "OPTIONS")
	authRouter.HandleFunc("/qrcode/{batch_id}", getQRCodeHandler).Methods("GET", "OPTIONS")
	authRouter.HandleFunc("/upload/image", uploadImageHandler).Methods("POST", "OPTIONS")
	// 添加检测结果上传功能
	authRouter.HandleFunc("/upload/detection", uploadDetectionHandler).Methods("POST", "OPTIONS")

	// 已移除YOLO检测相关功能

	// 同步相关接口 - 需要认证
	authRouter.HandleFunc("/sync/pruned", handlePrunedSyncRequest).Methods("POST", "OPTIONS")
	authRouter.HandleFunc("/sync/light", handleLightSyncRequest).Methods("POST", "OPTIONS")
	// 手动触发主节点间同步的接口
	authRouter.HandleFunc("/sync/main-nodes", manualSyncHandler).Methods("POST", "OPTIONS")

	// 静态文件服务
	qrcodeHandler := http.StripPrefix("/qrcode/",
		http.FileServer(http.Dir(filepath.Join(dataDir, "qrcodes"))))
	r.PathPrefix("/qrcode/").Handler(qrcodeHandler)

	server := &http.Server{
		Addr:         ":8090",
		Handler:      r,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	// 优雅关闭
	done := make(chan bool)
	go func() {
		quit := make(chan os.Signal, 1)
		signal.Notify(quit, os.Interrupt)
		<-quit

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(ctx)
		close(done)
	}()

	log.Println("Starting Product Chain Server on :8090")
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}

	<-done
	log.Println("Server stopped gracefully")
}
