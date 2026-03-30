# 服务启动指南

本目录包含两个程序：
- `api.py` - Flask应用（YOLO检测服务）
- `main.go` - Go Web服务（区块链溯源系统）

## 使用脚本启动服务

我已经为您创建了一个名为 `start_services.sh` 的Shell脚本，用于自动启动这两个服务并合并日志。

### 前置条件

1. **Linux系统** - 脚本适用于Linux环境
2. **Python和虚拟环境** - 确保已安装Python 3.8+并创建名为venv的虚拟环境
3. **Go环境** - 确保已安装Go 1.23+并配置好环境

## 环境配置指南

### 1. Go环境配置

#### 安装Go

1. 下载Go安装包（推荐Go 1.23+版本）：

```bash
# 对于64位Linux系统
wget https://go.dev/dl/go1.23.1.linux-amd64.tar.gz

# 或者使用curl
curl -O https://go.dev/dl/go1.23.1.linux-amd64.tar.gz
```

2. 解压安装包到`/usr/local`目录：

```bash
sudo tar -C /usr/local -xzf go1.23.1.linux-amd64.tar.gz
```

3. 配置环境变量：

编辑`~/.profile`或`~/.bashrc`文件：

```bash
nano ~/.bashrc
```

在文件末尾添加以下内容：

```bash
export PATH=$PATH:/usr/local/go/bin
export GOPATH=$HOME/go
export PATH=$PATH:$GOPATH/bin
```

4. 使配置生效：

```bash
source ~/.bashrc
```

5. 验证安装：

```bash
go version
```

如果安装成功，将显示Go的版本信息。

### 2. Python虚拟环境配置

Python 3.3+版本自带`venv`模块，可以用来创建虚拟环境。以下是在当前目录创建名为`venv`的虚拟环境的步骤：

1. 检查Python版本：

```bash
python3 --version
```

确保Python版本为3.8或更高。

2. 在当前目录创建名为`venv`的虚拟环境：

```bash
python3 -m venv venv
```

这个命令会在当前目录下创建一个名为`venv`的目录，包含Python解释器、标准库和其他支持文件。

3. 激活虚拟环境：

```bash
source venv/bin/activate
```

激活后，命令提示符会显示虚拟环境的名称（venv）。

4. 安装项目依赖：

```bash
# 确保requirements.txt文件存在
pip install -r requirements.txt
```

5. 退出虚拟环境：

```bash
deactivate
```

## 使用脚本启动服务

### 使用方法

1. 首先，确保脚本具有执行权限：

```bash
chmod +x start_services.sh
```

2. 然后，根据需要执行以下命令：

```bash
# 启动所有服务
./start_services.sh start

# 停止所有服务
./start_services.sh stop

# 重启所有服务
./start_services.sh restart

# 查看服务状态
./start_services.sh status

# 查看帮助信息
./start_services.sh help
```

### 脚本功能说明

- **自动激活虚拟环境** - 使用 `source venv/bin/activate` 命令
- **合并日志** - 所有服务的输出都会记录到 `services.log` 文件中
- **端口检查** - 启动前检查端口是否被占用（Flask默认8000端口，Go默认8080端口）
- **服务监控** - 可以查看服务状态和日志

## 手动启动方式（可选）

如果您不想使用脚本，也可以手动启动服务：

### 1. 启动Flask服务（api.py）

```bash
# 激活虚拟环境
source venv/bin/activate

# 安装依赖（如果尚未安装）
pip install -r requirements.txt

# 启动服务
python api.py
```

### 2. 启动Go服务（main.go）

```bash
# 安装依赖
go mod tidy

# 启动服务
go run main.go
```

## 重要注意事项

1. 确保虚拟环境已正确创建，并包含了api.py所需的所有Python包
2. 确保Go环境已正确配置
3. 默认情况下，Flask服务监听8000端口，Go服务监听8080端口
4. 日志文件保存在当前目录下的 `services.log`
5. 如果端口被占用，脚本会提示错误信息
6. 在生产环境中，请确保修改敏感配置（如JWT密钥等）

## 排错指南

如果服务启动失败，请检查以下几点：

1. 查看日志文件 `services.log` 获取详细错误信息
2. 确保所有依赖已正确安装
3. 确保端口未被其他程序占用
4. 确保文件权限设置正确

祝您使用愉快！