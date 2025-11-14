# Go多线程下载器 v3.1

一个高性能的下载工具，支持两种运行模式：
1.  **服务模式**：基于Redis队列的持续下载服务。
2.  **命令行模式**：用于单个文件的快速、直接下载。

它支持多线程下载、断点续传、自动重试和（在服务模式下）完全基于Redis的状态记录。

## 🏗️ 系统架构 (服务模式)

服务模式的架构非常简单和解耦，完全以Redis为中心：

```
+----------+     LPUSH      +------------------+     BLPOP      +-----------------+
|          |--------------->|                  |--------------->|                 |
| 客户端   |   任务数据     |      Redis       |    任务数据    |  Go 下载服务    |
| (任何语言)|<---------------| (go_download_urls) |<---------------|                 |
|          |   (可选)      |                  |   下载结果    |                 |
+----------+   检查结果    +------------------+     SET/HSET   +-----------------+
                             |
                             | 结果记录:
                             | - 成功: SET success:<url> (24h TTL)
                             | - 失败: HSET go_download_failed_tasks
                             |
```

## 🌟 主要特性

- ✅ **双模式运行** - 支持作为长期服务运行，或作为一次性命令行工具使用。
- ✅ **Redis队列驱动 (服务模式)** - 通过Redis List实现分布式的任务分发。
- ✅ **成功/失败状态记录 (服务模式)** - 下载的详细信息都会被记录回Redis。
- ✅ **智能多线程/单线程切换** - 根据服务器响应和网络状况自动调整下载策略。
- ✅ **网络适应性断点续传** - 断网或程序重启后，能从上次的进度继续下载。
- ✅ **网络异常智能重试** - 采用指数退避策略，优雅地处理临时网络问题。
- ✅ **实时进度显示** - 清晰地展示下载速度、ETA和完成率。
- ✅ **分组支持 (服务模式)** - 可通过分组名隔离不同类型的下载任务。

## 🔧 使用指南

### 模式一：服务模式 (推荐)

适用于需要持续、批量下载的生产环境。Go服务会作为后台进程持续监听Redis队列。

#### 步骤1：启动Go下载服务

使用 `go run` 或运行编译后的二进制文件来启动服务。所有配置都通过命令行flag传入。

```bash
# 添加可执行权限
chmod +x ./dist/go_downloader
cp ./dist/go_downloader /usr/local/bin/go_downloader
# 示例：使用自定义的Redis地址和10个并发下载 为任务设置分组"my-group"
go_downloader --redis-addr=my-redis:6379 --max-concurrent-downloads=10 --group=my-group

```
服务启动后，会显示当前配置并开始等待任务。

#### 步骤2：推送下载任务到Redis

通过任何Redis客户端，向 `go_download_urls` 列表中推送JSON格式的任务。

**Python示例脚本：**
```python
import redis, json

r = redis.Redis(host='localhost', port=6379, decode_responses=True)
task = {
    "url": "https://proof.ovh.net/files/10Gb.dat",
    "save_path": "./downloads/10Gb.dat"
}
# 如果Go服务启动时用了--group=my-group, 这里也要对应
queue_name = "my-group:go_download_urls"
r.lpush(queue_name, json.dumps(task))
print(f"✅ 任务已推送到Redis队列 '{queue_name}'")
```

#### 步骤3：监控与状态检查 (服务模式)

成功和失败的任务都会被记录在Redis中。

- **监控主队列**: `LLEN go_download_urls`
- **检查成功任务**: 成功记录以 `success:<URL>` 为键，24小时后过期。使用 `GET` 和 `TTL` 查看。
- **处理失败任务**: 失败记录存储在 `go_download_failed_tasks` 的Hash中。使用 `HGETALL`, `HGET`, `HDEL` 管理。

---

### 模式二：命令行模式

适用于单个、临时的下载任务，行为类似于 `curl` 或 `wget`。

#### 使用方法

直接在程序名后跟上 `URL` 和 `保存路径` 即可。

```bash
# 命令格式: go run main.go <URL> <SavePath>
go_downloader https://proof.ovh.net/files/1Gb.dat ./downloads/1Gb.dat
```
程序将在下载完成后自动退出。

---
## ⚙️ 服务模式参数 (Flags)

*注意：以下参数仅在服务模式下生效。*

| Flag                       | 类型   | 默认值                  | 描述                 |
| -------------------------- | ------ | ----------------------- | -------------------- |
| `--redis-addr`             | string | `"localhost:6379"`      | Redis服务器地址      |
| `--redis-password`         | string | `""`                    | Redis密码            |
| `--max-concurrent-downloads` | int    | `5`                     | 最大并发下载数       |
| `--group`                  | string | `""`                    | 任务分组名，用作Redis Key的前缀 |

## 📦 构建和打包

您可以将Go程序编译成一个独立的二进制文件。

#### 1. 构建

```bash
# 这将生成一个名为 go_downloader (Linux/macOS) 或 go_downloader.exe (Windows) 的可执行文件
go build -o ./dist/go_downloader.exe main.go

# 在windows上变liunx可执行文件
set GOARCH=amd64
go env -w GOARCH=amd64
set GOOS=linux
go env -w GOOS=linux
go build -o ./dist/go_downloader main.go
```

## 🔍 故障排除

1. **编译失败**
   - 确保您已安装Go语言环境 (1.18或更高版本)。
   - 运行 `go mod tidy` 下载依赖。

2. **Redis连接失败 (服务模式)**
   - 检查Redis服务是否启动。
   - 确认地址、端口和密码是否正确。

3. **下载失败**
   - 检查网络连接。
   - 确认目标URL可访问。
   - 查看Go程序的错误日志。
   - 在服务模式下，可在Redis的 `go_download_failed_tasks` Hash中找到失败详情。

##  许可证

MIT License