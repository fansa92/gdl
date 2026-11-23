# gdl — 命令行下载器

用 Go 编写的单文件下载工具。核心是「多连接下载 + 分块级断点续传」，同时补齐了日常脚本里常用的条件请求、认证、Cookie、限速与批量任务。

依赖只有标准库和 `golang.org/x/net`（socks5 代理、公共后缀列表），编译产物是单个静态可执行文件。

## 构建

```powershell
go build -o gdl.exe .
go test ./...
```

## 用法

```text
gdl [选项] <URL> [URL...]

基础:
  -o, -O, -output     输出文件或目录（- 表示写到 stdout）
  -c,  -connections   并发分块数（0=自动，默认取 CPU 核数 4~16）
  -r,  -retries       每个分块的最大重试次数（默认 5）
  -t,  -timeout       空闲超时，多久收不到数据就重试（默认 30s）
  -f,  -force         覆盖已存在的文件并重新下载
  -nc, -no-clobber    文件已存在时跳过（退出码 0）
  -N,  -timestamping  条件请求：本地已是最新则跳过（304）
  -s,  -sum           下载后校验哈希：sha256|sha512|sha1|md5
  -i,  -input-file    从文件读取 URL 清单（- 读 stdin）
  -j,  -jobs          同时下载的任务数（默认 1）

请求:
  -U,  -agent         自定义 User-Agent
  -H,  -header        自定义请求头，可重复
  -u,  -user          Basic 认证 user[:password]
       -bearer        Bearer token
       -cookie        直接发送 Cookie 头
       -load-cookies  读取 Netscape 格式 cookie 文件
       -cookie-jar    下载结束后保存 cookie
  -M,  -mirror        镜像 URL，可重复，按顺序尝试

网络:
  -L,  -rate-limit    限速，如 5M/100k/1G
  -P,  -proxy         代理 http:// / https:// / socks5://
  -k,  -insecure      跳过 TLS 证书验证
       -ca-cert       自定义 CA 证书文件
       -cert / -key   客户端证书与私钥（双向 TLS）
  -4 / -6             仅用 IPv4 / IPv6
       -interface     绑定出口网卡
       -http2         HTTP/2: auto|on|off
       -max-redirs    最大重定向次数（0=默认 10，-1=不限）

限制:
       -max-filesize  大小上限，如 100M
       -min-filesize  大小下限，如 1k
       -speed-limit   速度下限，低于该值持续 -speed-time 后中断
       -speed-time    观察窗口（默认 10s）

显示与工具:
  -q,  -quiet         安静模式
       -progress      进度条风格: auto|ascii|unicode|none
       -color         颜色: auto|always|never
  -w,  -write-out     下载结束后输出模板
       -completion    生成补全脚本: bash|zsh|fish|powershell
       -version       显示版本号
```

示例：

```powershell
gdl https://example.com/file.zip
gdl -c 16 -o .\file.zip https://example.com/file.zip
gdl -O - https://example.com/file.tar.gz | tar xz            # 管道，不解落盘
gdl -N -o .\index.html https://example.com/index.html        # 未更新就跳过
gdl -u alice:secret -o .\priv.zip https://example.com/priv.zip
gdl -i urls.txt -j 4 -o .\downloads                          # 批量并发
gdl -s sha256 -f -L 2M https://example.com/file.zip
gdl -H "Referer: https://example.com/" -H "Cookie: a=b" https://example.com/f.zip
gdl -M https://m1/f.zip -M https://m2/f.zip https://a.com/f.zip   # 主源挂了自动换镜像
gdl -w "%{http_code} %{size_download} %{speed_download}\n" https://example.com/f.zip
gdl --completion bash > /usr/share/bash-completion/completions/gdl
```

## 退出码

| 码 | 含义 |
| --- | --- |
| 0 | 成功（含 304 未修改、`-nc` 跳过） |
| 1 | 未分类错误 |
| 2 | 用法 / 参数错误 |
| 3 | 目标文件已存在且未加 `-f` |
| 4 | 网络、超时、TLS、重定向超限 |
| 5 | 服务器返回 4xx / 5xx |
| 6 | 校验失败（哈希不符、长度不符） |
| 7 | 触碰限制（大小上限、速度下限） |
| 8 | 本地文件 / 目录不可写 |
| 130 | 用户中断（Ctrl+C） |

## 和 wget / wget2 / curl 的对应关系

| 能力 | gdl | wget | wget2 | curl |
| --- | --- | --- | --- | --- |
| 单文件多连接分块 | `-c`（默认开） | 无 | 需显式开启 | 无 |
| 断点续传 | 分块级 `.part.json`，校验 ETag / Last-Modified | `-c`，只看文件长度 | 支持 | `-C`，只看文件长度 |
| 失败重试 | 分块级指数退避 | `--tries` | 支持 | `--retry` |
| 空闲超时 | `-t` 独立看门狗 | `--read-timeout` | 支持 | `--speed-time` |
| 速度下限中断 | `-speed-limit` | 无 | 无 | `--speed-limit` |
| 大小上限/下限 | 有 | 无 | 无 | `--max-filesize`（仅上限） |
| 镜像 / 多源回退 | `-M`，切换时保留已完成分块 | 无 | 无 | 无 |
| 条件请求 | `-N`（304 跳过 + 回写 mtime） | `-N` | 支持 | `-z` |
| 认证 | Basic / Bearer | Basic / Digest | 同 wget | 全（含 NTLM） |
| Cookie | `-cookie` / `-load-cookies` / `-cookie-jar` | 支持 | 支持 | 支持 |
| 批量清单 | `-i` | `-i` | 支持 | `-K`（配置文件） |
| 并发多任务 | `-j` | 无 | 支持 | `--parallel` |
| 哈希校验 | 内置 4 种算法 | 无 | Metalink 场景 | 无 |
| 递归 / 镜像网站 | 不做 | 核心能力 | 核心能力 | 不做 |
| 上传 / POST / 25+ 协议 | 不做 | 部分（FTP） | 部分 | 核心能力 |
| Shell 补全生成 | 内置 | 无 | 无 | 无 |

gdl 独有的部分：`.part.json` 分块断点 + ETag 校验、镜像切换保留进度、内置哈希校验、默认拒绝覆盖的保守写入策略、Windows 控制台 UTF-8 处理。

## 工作方式

1. **探测**：先发 `HEAD`，再用 `Range: bytes=0-0` 确认服务器是否支持断点，拿到文件大小、`ETag`、`Last-Modified` 和 `Content-Disposition` 文件名。探测阶段同样带重试。加了 `-N` 时探测请求会带 `If-Modified-Since`，命中 304 直接返回。
2. **分块并发**：服务器支持 `Range` 且 `-c > 1` 时，把文件切成 N 段并行下载，每段独立重试（指数退避），数据用 `WriteAt` 直接写进同一个预分配好的临时文件。管道模式（`-O -`）只有单流：字节写出去就收不回，因此失败不重试。
3. **进度**：进度条渲染到 stderr（`\r` 原地刷新，含百分比、速率、ETA），所以重定向 stdout 不会受影响；`-j` 并发时自动关闭进度条，改为逐行输出结果。
4. **断点续传**：每秒把各分块的完成字节数写入 `<文件>.part.json`，数据落在 `<文件>.part`。重新执行同一命令会校验 `ETag` / 大小是否变化，一致则续传，不一致则重新下载。下载成功后这两个文件自动删除。
5. **收尾**：`Sync` 落盘 → 校验总长度 → 大小上下限 → 可选哈希 → 重命名 `<文件>.part` 为正式文件名 → `-N` 时把 `Last-Modified` 写回本地 mtime。

## 特殊情况处理

| 情况 | 行为 |
| --- | --- |
| 服务器中途改口不支持 Range | 清理状态，自动退回单流模式重来一次 |
| 服务器忽略 Range（返回 200） | 单流下载时从头重下；分块模式则退回单流 |
| 长度未知（chunked 响应） | 按 EOF 判断结束，续传时若服务器不支持 Range 就重下 |
| 文件已存在 | 默认报错退出（码 3）；`-f` 覆盖、`-nc` 跳过、`-N` 走条件请求 |
| 输出是 `-o dir/` 或已存在目录 | 按 `Content-Disposition` 文件名落盘，缺失时用 URL 末段 |
| 下载中触及大小上限 | 立即中断，退出码 7，不留半成品（`.part` 仍在） |
| 速度持续低于 `-speed-limit` | 中断，退出码 7，可重跑续传 |
| Ctrl+C 或进程被杀 | 保留 `.part` 与 `.part.json`，下次继续 |
| 镜像主源失败 | 切下一个镜像，已下完的分块不重下 |
| 跨域重定向 | 不携带 `Authorization`，最多 `-max-redirs` 次 |

## 已知限制

- 只支持 HTTP / HTTPS，不做 FTP / SFTP / WebSocket / HTTP/3。
- 不做递归抓取与链接改写（那属于爬虫领域，用 wget2 更合适）。
- 不支持 POST / PUT / 分片上传。
- `-N` 与 `-c` 续传互斥：命中 `-N` 时不使用断点状态，按条件请求重新校验。

## 项目结构

```text
main.go                        CLI 入口：参数解析、批量清单、并发调度、结果输出
console_windows.go / _other.go Windows 下临时把控制台代码页切到 UTF-8，避免中文乱码
internal/config/
  config.go                    配置文件 + GDL_* 环境变量 + CLI 三层合并，大小支持 "100M" 写法
internal/download/
  download.go                  下载主流程：分块规划、并发、重试、stdout 模式、收尾
  probe.go                     资源探测、条件请求、客户端构造、限速、文件名解析
  state.go                     断点状态的读写与校验
  client.go                    HTTP 客户端与拨号（超时 / IPv4-6 / 出口网卡）
  cookies.go                   Netscape cookie 文件读写
  errors.go                    退出码体系
internal/progress/
  progress.go                  进度条与字节/时长格式化
internal/report/
  report.go                    --write-out 模板渲染
```
