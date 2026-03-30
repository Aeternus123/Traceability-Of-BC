#!/bin/bash

# 定义日志文件路径
LOG_FILE="services.log"

# 定义虚拟环境路径
VENV_PATH="venv"

# 函数：检查端口是否被占用
check_port() {
    local port=$1
    if lsof -Pi :$port -sTCP:LISTEN -t >/dev/null ; then
        echo "错误：端口 $port 已被占用，可能是服务已经在运行中。"
        echo "请先停止占用该端口的进程，或修改服务配置使用其他端口。"
        return 1
    fi
    return 0
}

# 函数：启动服务并记录日志
start_service() {
    local cmd=$1
    local service_name=$2
    
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] 正在启动 $service_name..." | tee -a $LOG_FILE
    
    # 在后台启动服务，并将标准输出和错误输出都追加到日志文件
    $cmd >> $LOG_FILE 2>&1 &
    
    local pid=$!
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] $service_name 已启动，进程ID: $pid" | tee -a $LOG_FILE
    
    # 等待2秒，检查服务是否正常启动
    sleep 2
    
    if ps -p $pid > /dev/null; then
        echo "[$(date '+%Y-%m-%d %H:%M:%S')] $service_name 启动成功！" | tee -a $LOG_FILE
        return 0
    else
        echo "[$(date '+%Y-%m-%d %H:%M:%S')] 错误：$service_name 启动失败！" | tee -a $LOG_FILE
        # 显示日志的最后几行，以便排查问题
        echo "最近的日志内容："
        tail -n 20 $LOG_FILE
        return 1
    fi
}

# 函数：停止所有服务
stop_services() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] 正在停止所有服务..." | tee -a $LOG_FILE
    
    # 停止Flask服务（通常监听在8000端口）
    flask_pid=$(lsof -Pi :8000 -sTCP:LISTEN -t)
    if [ -n "$flask_pid" ]; then
        kill $flask_pid
        echo "[$(date '+%Y-%m-%d %H:%M:%S')] Flask服务已停止，进程ID: $flask_pid" | tee -a $LOG_FILE
    fi
    
    # 停止Go服务（通常监听在8080端口）
    go_pid=$(lsof -Pi :8080 -sTCP:LISTEN -t)
    if [ -n "$go_pid" ]; then
        kill $go_pid
        echo "[$(date '+%Y-%m-%d %H:%M:%S')] Go服务已停止，进程ID: $go_pid" | tee -a $LOG_FILE
    fi
    
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] 所有服务停止完成。" | tee -a $LOG_FILE
}

# 函数：显示帮助信息
show_help() {
    echo "用法：$0 [start|stop|restart|status]"
    echo ""
    echo "选项："
    echo "  start    启动所有服务"
    echo "  stop     停止所有服务"
    echo "  restart  重启所有服务"
    echo "  status   显示服务状态"
    echo "  help     显示帮助信息"
    echo ""
    echo "示例："
    echo "  $0 start   # 启动所有服务"
    echo "  $0 stop    # 停止所有服务"
}

# 函数：显示服务状态
show_status() {
    echo "服务状态（$(date '+%Y-%m-%d %H:%M:%S')）："
    echo "----------------------------------------"
    
    # 检查Flask服务状态
    flask_pid=$(lsof -Pi :8000 -sTCP:LISTEN -t)
    if [ -n "$flask_pid" ]; then
        echo "✓ Flask服务 (api.py) 正在运行，进程ID: $flask_pid，端口: 8000"
    else
        echo "✗ Flask服务 (api.py) 未运行"
    fi
    
    # 检查Go服务状态
    go_pid=$(lsof -Pi :8080 -sTCP:LISTEN -t)
    if [ -n "$go_pid" ]; then
        echo "✓ Go服务 (main.go) 正在运行，进程ID: $go_pid，端口: 8080"
    else
        echo "✗ Go服务 (main.go) 未运行"
    fi
    
    echo "----------------------------------------"
    echo "日志文件: $LOG_FILE"
}

# 主程序开始

# 清空旧日志文件
> $LOG_FILE

# 根据参数执行不同的操作
case "$1" in
    start)
        echo "[$(date '+%Y-%m-%d %H:%M:%S')] 开始启动所有服务..." | tee -a $LOG_FILE
        echo "----------------------------------------" >> $LOG_FILE
        
        # 检查端口是否被占用
        if ! check_port 8000 || ! check_port 8080; then
            exit 1
        fi
        
        # 检查虚拟环境是否存在
        if [ ! -d "$VENV_PATH" ]; then
            echo "错误：虚拟环境 $VENV_PATH 不存在！" | tee -a $LOG_FILE
            echo "请先创建虚拟环境，命令示例：python -m venv venv" | tee -a $LOG_FILE
            exit 1
        fi
        
        # 激活虚拟环境
        echo "[$(date '+%Y-%m-%d %H:%M:%S')] 正在激活虚拟环境..." | tee -a $LOG_FILE
        source $VENV_PATH/bin/activate
        
        if [ $? -ne 0 ]; then
            echo "错误：激活虚拟环境失败！" | tee -a $LOG_FILE
            exit 1
        fi
        
        echo "[$(date '+%Y-%m-%d %H:%M:%S')] 虚拟环境激活成功。" | tee -a $LOG_FILE
        
        # 启动Flask服务（api.py）
        start_service "$VENV_PATH/bin/python3 api.py" "Flask服务 (api.py)"
        flask_status=$?
        
        if [ $flask_status -ne 0 ]; then
            echo "启动Flask服务失败，取消后续操作。" | tee -a $LOG_FILE
            exit 1
        fi
        
        # 启动Go服务（main.go）
        # 先检查是否有go.mod文件，确保这是一个Go模块
        if [ -f "go.mod" ]; then
            # 确保依赖已安装
            echo "[$(date '+%Y-%m-%d %H:%M:%S')] 正在检查并安装Go依赖..." | tee -a $LOG_FILE
            go mod tidy >> $LOG_FILE 2>&1
            
            # 启动Go服务
            start_service "go run main.go" "Go服务 (main.go)"
            go_status=$?
            
            if [ $go_status -ne 0 ]; then
                echo "启动Go服务失败，正在停止已启动的服务..." | tee -a $LOG_FILE
                stop_services
                exit 1
            fi
        else
            echo "错误：未找到go.mod文件，无法启动Go服务。" | tee -a $LOG_FILE
            stop_services
            exit 1
        fi
        
        echo "[$(date '+%Y-%m-%d %H:%M:%S')] 所有服务启动成功！" | tee -a $LOG_FILE
        echo "----------------------------------------" >> $LOG_FILE
        
        # 显示服务状态
        show_status
        
        echo ""
        echo "日志正在记录到 $LOG_FILE，您可以使用 'tail -f $LOG_FILE' 命令查看实时日志。"
        echo "使用 '$0 stop' 命令可以停止所有服务。"
        ;;
    stop)
        stop_services
        ;;
    restart)
        stop_services
        # 等待3秒，确保所有进程都已停止
        sleep 3
        $0 start
        ;;
    status)
        show_status
        ;;
    help|"")
        show_help
        ;;
    *)
        echo "错误：未知参数 '$1'" >&2
        show_help
        exit 1
        ;;
esac

# 注意事项：
# 1. 请确保您的系统已安装所需的依赖（Python、Go等）
# 2. 请确保虚拟环境已创建，并且包含了api.py所需的所有Python包
# 3. 日志文件默认保存在当前目录下的services.log
# 4. 默认情况下，Flask服务监听8000端口，Go服务监听8080端口