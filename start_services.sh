#!/bin/bash

# 定义日志文件路径
LOG_FILE="services.log"

# 定义服务端口
YOLO_SERVER_PORT=8000
MAIN_GO_PORT=5500

# ROS相关配置
ROS_CORE_PORT=11311

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

# 函数：检查ROS环境
check_ros_environment() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] 检查ROS环境..." | tee -a $LOG_FILE
    
    # 检查ROS是否安装
    if ! command -v roscore &> /dev/null; then
        echo "错误：未找到roscore命令，ROS环境可能未正确安装或未添加到PATH" | tee -a $LOG_FILE
        return 1
    fi
    
    # 检查ROS环境变量
    if [ -z "$ROS_ROOT" ]; then
        echo "警告：未设置ROS环境变量，尝试使用source指令初始化" | tee -a $LOG_FILE
        
        # 尝试找到并source ROS环境
        ROS_SETUP_FILE=""
        if [ -f "/opt/ros/noetic/setup.bash" ]; then
            ROS_SETUP_FILE="/opt/ros/noetic/setup.bash"
        elif [ -f "/opt/ros/melodic/setup.bash" ]; then
            ROS_SETUP_FILE="/opt/ros/melodic/setup.bash"
        elif [ -f "/opt/ros/kinetic/setup.bash" ]; then
            ROS_SETUP_FILE="/opt/ros/kinetic/setup.bash"
        fi
        
        if [ -n "$ROS_SETUP_FILE" ]; then
            echo "使用ROS环境: $ROS_SETUP_FILE" | tee -a $LOG_FILE
            source "$ROS_SETUP_FILE"
        else
            echo "警告：未找到标准位置的ROS setup文件，请确保已手动source ROS环境" | tee -a $LOG_FILE
        fi
    fi
    
    return 0
}

# 函数：启动ROS核心
start_ros_core() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] 正在启动ROS核心..." | tee -a $LOG_FILE
    
    # 检查端口是否被占用
    if lsof -Pi :$ROS_CORE_PORT -sTCP:LISTEN -t >/dev/null ; then
        echo "警告：ROS核心端口 $ROS_CORE_PORT 已被占用，可能已有roscore在运行" | tee -a $LOG_FILE
        return 0
    fi
    
    # 启动roscore
    roscore > /dev/null 2>&1 &
    ROSCORE_PID=$!
    
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] ROS核心已启动，进程ID: $ROSCORE_PID" | tee -a $LOG_FILE
    
    # 等待ROS核心初始化完成
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] 等待ROS核心初始化完成..." | tee -a $LOG_FILE
    sleep 3
    
    # 检查roscore是否仍在运行
    if ps -p $ROSCORE_PID > /dev/null; then
        echo "[$(date '+%Y-%m-%d %H:%M:%S')] ROS核心启动成功！" | tee -a $LOG_FILE
        return 0
    else
        echo "[$(date '+%Y-%m-%d %H:%M:%S')] 错误：ROS核心启动失败！" | tee -a $LOG_FILE
        return 1
    fi
}

# 函数：停止所有服务
stop_services() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] 正在停止所有服务..." | tee -a $LOG_FILE
    
    # 查找并停止yoloserver.py服务进程
    yoloserver_pid=$(ps aux | grep "python.*./yoloserver.py" | awk '{print $2}')
    if [ -n "$yoloserver_pid" ]; then
        kill $yoloserver_pid
        echo "[$(date '+%Y-%m-%d %H:%M:%S')] yoloserver.py服务已停止，进程ID: $yoloserver_pid" | tee -a $LOG_FILE
    fi
    
    # 查找并停止ROS核心进程
    roscore_pid=$(ps aux | grep "roscore" | grep -v "grep" | awk '{print $2}')
    if [ -n "$roscore_pid" ]; then
        echo "[$(date '+%Y-%m-%d %H:%M:%S')] 停止ROS核心进程..." | tee -a $LOG_FILE
        kill $roscore_pid
        echo "[$(date '+%Y-%m-%d %H:%M:%S')] ROS核心已停止，进程ID: $roscore_pid" | tee -a $LOG_FILE
    fi
    
    # 查找并停止myapp服务进程
    myapp_pid=$(ps aux | grep "./myapp" | awk '{print $2}')
    if [ -n "$myapp_pid" ]; then
        kill $myapp_pid
        echo "[$(date '+%Y-%m-%d %H:%M:%S')] myapp服务已停止，进程ID: $myapp_pid" | tee -a $LOG_FILE
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

# 函数：检查Python库是否安装
check_python_library() {
    local library=$1
    if ! python -c "import $library" 2>/dev/null && ! python3 -c "import $library" 2>/dev/null; then
        echo "警告：Python库 '$library' 未安装"
        return 1
    fi
    return 0
}

# 函数：安装Python库
install_python_library() {
    local library=$1
    echo "正在安装Python库: $library"
    if command -v pip3 &> /dev/null; then
        pip3 install $library
    elif command -v pip &> /dev/null; then
        pip install $library
    else
        echo "错误：未找到pip，无法安装Python库"
        return 1
    fi
    return $?
}

# 函数：检查ROS核心状态
check_ros_core_status() {
    # 检查roscore进程是否在运行
    roscore_pid=$(ps aux | grep "roscore" | grep -v "grep" | awk '{print $2}')
    
    if [ -n "$roscore_pid" ]; then
        echo "ROS核心: 运行中 (PID: $roscore_pid)" | tee -a $LOG_FILE
        return 0
    else
        echo "ROS核心: 未运行" | tee -a $LOG_FILE
        return 1
    fi
}

# 函数：显示服务状态
show_status() {
    echo "服务状态（$(date '+%Y-%m-%d %H:%M:%S')）：" | tee -a $LOG_FILE
    echo "----------------------------------------" | tee -a $LOG_FILE
    
    # 检查ROS核心状态
    echo "ROS核心状态:" | tee -a $LOG_FILE
    roscore_pid=$(ps aux | grep "roscore" | grep -v "grep" | awk '{print $2}')
    if [ -n "$roscore_pid" ]; then
        echo "✓ ROS核心 正在运行，进程ID: $roscore_pid" | tee -a $LOG_FILE
    else
        echo "✗ ROS核心 未运行" | tee -a $LOG_FILE
    fi
    
    echo "----------------------------------------" | tee -a $LOG_FILE
    echo "服务状态:" | tee -a $LOG_FILE
    # 检查myapp服务状态
    myapp_pid=$(ps aux | grep "\./myapp" | grep -v "grep" | awk '{print $2}')
    if [ -n "$myapp_pid" ]; then
        echo "✓ myapp服务 正在运行，进程ID: $myapp_pid" | tee -a $LOG_FILE
    else
        echo "✗ myapp服务 未运行" | tee -a $LOG_FILE
    fi
    
    # 检查yoloserver.py服务状态
    yoloserver_pid=$(ps aux | grep "python.*./yoloserver.py" | grep -v "grep" | awk '{print $2}')
    if [ -n "$yoloserver_pid" ]; then
        echo "✓ yoloserver.py服务 正在运行，进程ID: $yoloserver_pid" | tee -a $LOG_FILE
    else
        echo "✗ yoloserver.py服务 未运行" | tee -a $LOG_FILE
    fi
    
    echo "----------------------------------------" | tee -a $LOG_FILE
    echo "端口状态:" | tee -a $LOG_FILE
    # 检查端口状态
    if lsof -Pi :$MAIN_GO_PORT -sTCP:LISTEN -t >/dev/null ; then
        echo "✓ 端口 $MAIN_GO_PORT (main.go) 正在监听" | tee -a $LOG_FILE
    else
        echo "✗ 端口 $MAIN_GO_PORT (main.go) 未监听" | tee -a $LOG_FILE
    fi
    
    if lsof -Pi :$YOLO_SERVER_PORT -sTCP:LISTEN -t >/dev/null ; then
        echo "✓ 端口 $YOLO_SERVER_PORT (yoloserver.py) 正在监听" | tee -a $LOG_FILE
    else
        echo "✗ 端口 $YOLO_SERVER_PORT (yoloserver.py) 未监听" | tee -a $LOG_FILE
    fi
    
    if lsof -Pi :$ROS_CORE_PORT -sTCP:LISTEN -t >/dev/null ; then
        echo "✓ 端口 $ROS_CORE_PORT (ROS核心) 正在监听" | tee -a $LOG_FILE
    else
        echo "✗ 端口 $ROS_CORE_PORT (ROS核心) 未监听" | tee -a $LOG_FILE
    fi
    
    echo "----------------------------------------" | tee -a $LOG_FILE
    echo "日志文件: $LOG_FILE" | tee -a $LOG_FILE
}

# 主程序开始

# 清空旧日志文件
> $LOG_FILE

# 根据参数执行不同的操作
case "$1" in
    start)
        echo "[$(date '+%Y-%m-%d %H:%M:%S')] 开始启动所有服务..." | tee -a $LOG_FILE
        echo "----------------------------------------" >> $LOG_FILE
        
        # 检查ROS环境
    if ! check_ros_environment; then
        echo "ROS环境检查失败，尝试继续..." | tee -a $LOG_FILE
        # 继续执行，因为用户可能已经手动设置了ROS环境
    fi
    
    # 启动ROS核心
    if ! start_ros_core; then
        echo "警告：ROS核心启动失败，可能会影响依赖ROS的服务" | tee -a $LOG_FILE
        # 继续执行，让用户决定是否要停止
    fi
    
    # 检查端口是否被占用
    if ! check_port $MAIN_GO_PORT; then
        exit 1
    fi
    if ! check_port $YOLO_SERVER_PORT; then
        exit 1
    fi
        
        # 启动myapp服务（使用--service参数避免命令行交互）
        echo "[$(date '+%Y-%m-%d %H:%M:%S')] 正在启动myapp服务..." | tee -a $LOG_FILE
        start_service "./myapp --service" "myapp服务"
        myapp_status=$?
        
        if [ $myapp_status -ne 0 ]; then
            echo "启动myapp服务失败，取消后续操作。" | tee -a $LOG_FILE
            exit 1
        fi
        
        # 检查ROS核心状态，如果没有运行则发出警告
        if ! check_ros_core_status; then
            echo "警告：ROS核心未运行，这可能会影响依赖ROS的服务功能" | tee -a $LOG_FILE
        fi
        
        # 检查Python是否安装
        if ! command -v python &> /dev/null && ! command -v python3 &> /dev/null; then
            echo "错误：未找到Python，无法启动arm.py服务。" | tee -a $LOG_FILE
            stop_services
            exit 1
        fi
        
        # 确定使用哪个Python命令
        if command -v python3 &> /dev/null; then
            PYTHON_CMD="python3"
        else
            PYTHON_CMD="python"
        fi
        
        # 检查并安装必要的Python库
        echo "[$(date '+%Y-%m-%d %H:%M:%S')] 检查必要的Python库..." | tee -a $LOG_FILE
        if ! check_python_library "ultralytics"; then
            echo "[$(date '+%Y-%m-%d %H:%M:%S')] 安装Ultralytics库..." | tee -a $LOG_FILE
            if ! install_python_library "ultralytics"; then
                echo "安装Ultralytics库失败，请手动安装" | tee -a $LOG_FILE
                # 继续尝试启动服务，因为用户可能已经通过其他方式安装
            fi
        fi
        
        # 检查其他必要的库
        for lib in "rospy" "cv2" "torch" "numpy" "requests" "cv_bridge"; do
            if ! check_python_library "$lib"; then
                echo "警告：未安装Python库 '$lib'，请确保在运行前已安装所有依赖" | tee -a $LOG_FILE
            fi
        done
        
        # 等待main.go服务完全启动
        echo "[$(date '+%Y-%m-%d %H:%M:%S')] 等待main.go服务初始化完成..." | tee -a $LOG_FILE
        sleep 3
        
        # 启动yoloserver.py服务
        echo "[$(date '+%Y-%m-%d %H:%M:%S')] 正在启动yoloserver.py服务..." | tee -a $LOG_FILE
        start_service "$PYTHON_CMD ./yoloserver.py" "yoloserver.py服务"
        yoloserver_status=$?
        
        if [ $yoloserver_status -ne 0 ]; then
            echo "启动yoloserver.py服务失败，正在停止已启动的服务..." | tee -a $LOG_FILE
            stop_services
            exit 1
        fi
        
        echo "[$(date '+%Y-%m-%d %H:%M:%S')] 所有服务启动成功！" | tee -a $LOG_FILE
        echo "----------------------------------------" >> $LOG_FILE
        
        # 显示服务状态
        show_status
        
        echo ""
    echo "日志正在记录到 $LOG_FILE，您可以使用 'tail -f $LOG_FILE' 命令查看实时日志。"
    echo "使用 '$0 stop' 命令可以停止所有服务，包括ROS核心。"
    echo "注意：main.go服务运行在 http://localhost:$MAIN_GO_PORT"
    echo "      yoloserver.py服务运行在 http://localhost:$YOLO_SERVER_PORT"
    echo "      ROS核心运行在端口 $ROS_CORE_PORT"
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
# 1. 请确保您的系统已安装所需的依赖（Python、Ultralytics、ROS等）
# 2. 日志文件默认保存在当前目录下的services.log
# 3. 服务启动顺序：先启动ROS核心，再启动main.go，最后启动yoloserver.py
# 4. main.go服务默认监听$MAIN_GO_PORT端口
# 5. yoloserver.py服务默认监听$YOLO_SERVER_PORT端口
# 6. ROS核心默认监听$ROS_CORE_PORT端口
# 7. 使用stop命令会停止所有服务，包括ROS核心