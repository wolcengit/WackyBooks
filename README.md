# WackyBooks

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

WackyBooks 是一个自托管电子书库。它扫描服务器上的书籍目录，提供网页书架、在线阅读、收藏和按书库授权。后端使用 Go，Vue 3 前端已随服务打包。

## 项目状态

项目仍在持续开发中，功能和配置可能随版本调整。使用前请阅读本文中的部署和数据备份说明。

## 文档导航

- [贡献指南](CONTRIBUTING.md)：提交问题、开发和提交变更的流程。
- [行为准则](CODE_OF_CONDUCT.md)：项目协作中的基本规范。
- [安全政策](SECURITY.md)：报告安全问题的方式。
- [MIT 许可证](LICENSE)：软件的使用和分发条款。
- [设计文档](docs/design/)：早期需求与实现方案参考，可能与当前实现不同。

项目地址：[github.com/wolcengit/WackyBooks](https://github.com/wolcengit/WackyBooks)。问题反馈和功能建议请通过 GitHub Issues 提交。

## 快速开始

需要 Go 1.25 或更新版本。Linux 上可选装 Poppler 的 `pdfinfo` 和 `pdftoppm`，用于读取 PDF 元数据、生成封面和逐页阅读。使用 Docker 时镜像已内置这些工具。

在项目根目录运行以下命令。

```bash
go run ./app
```

1. 在浏览器打开 `http://127.0.0.1:8080`，按页面提示设置本地 `Admin` 的密码。
2. 进入「后台管理 → 新增书库」，填写书库名称和服务器上可读的绝对目录，例如 `/srv/books`。
3. 在左侧选择书库，通过目录树浏览。书籍有增删时，在后台点击「重新扫描」。

## 构建和运行单文件

在项目根目录执行以下命令，生成并启动适用于当前操作系统和 CPU 架构的可执行文件。前端资源已内嵌，部署时可以只复制二进制文件。

```bash
CGO_ENABLED=0 go build -mod=readonly -trimpath -ldflags="-s -w" -o wackybooks ./app
./wackybooks
```

在 Linux 上可设置 `GOOS=linux` 和 `GOARCH=amd64` 或 `GOARCH=arm64`，为对应架构交叉编译。运行时可指定监听地址和数据目录。下面的示例在本机端口 `8090` 启动。

```bash
WACKY_ADDR=127.0.0.1:8090 WACKY_DATA="./data" ./wackybooks
```

前台运行时按 `Ctrl+C` 停止。相对数据目录以启动程序时所在的目录为基准，部署时可使用绝对路径来固定数据位置。

## Docker

### 构建和启动容器

构建镜像并启动容器。示例把当前目录下的 `books` 挂载为只读书库，并将应用状态保存在 Docker 卷中。

```bash
mkdir -p books
docker build -t wackybooks .
docker run -d --name wackybooks \
  -p 127.0.0.1:8080:8080 \
  -v wackybooks-data:/data \
  -v "$(pwd)/books:/books:ro" \
  wackybooks
```

进入后台添加书库时填写容器内的路径 `/books`。镜像使用 UID 和 GID `10001` 运行，宿主机上的书库文件需对该用户可读，目录需可遍历。运行镜像基于 Alpine，包含系统 CA 证书和用于 PDF 元数据、封面与逐页阅读的 Poppler 工具。

容器内监听 `0.0.0.0:8080`，示例将它发布到宿主机的 `127.0.0.1:8080`。如果宿主机端口已被占用，可将端口映射改为 `-p 127.0.0.1:8090:8080`，然后访问 `http://127.0.0.1:8090`。

```bash
docker logs -f wackybooks
docker stop wackybooks
docker start wackybooks
```

如需把状态直接保存到宿主机目录，可将 `-v wackybooks-data:/data` 替换为 `-v "$(pwd)/data:/data"`。先创建该目录并确保容器用户 `10001:10001` 可读写目录及现有状态文件。重建容器时使用同一个卷或宿主机目录即可继续读取已有状态。

### 用 Docker 导出单文件

如需通过 Docker 仅导出内嵌前端的 Linux 单文件，可构建 `binary` 阶段。产物为 `dist/wackybooks`。

```bash
docker buildx build --target binary --output type=local,dest=dist .
```

在架构匹配的 Linux 主机上运行导出的文件。

```bash
./dist/wackybooks
```

## 浏览和阅读

- 通过「超级搜索」在有权访问的书库中查询书名、作者、出版社、主题、简介、语言、标识符、日期和文件路径。可以按书库和格式筛选，按书名或收藏人数排序。
- 首页分别显示最近阅读和最近新增的最多 10 本书。最近新增按书籍文件修改时间排序。
- 点击封面在线阅读，阅读页可下载原文件。最近阅读显示上次阅读时间、进度和累计时长。时长按阅读页在前台且窗口处于焦点的时间估算，阅读中约每 15 秒保存一次。
- 每条收藏清单记录保存收藏时间。一本书被同一读者加入多个清单时，收藏人数只计一次。

| 格式 | 阅读与进度 |
| --- | --- |
| PDF | 使用 Poppler 逐页阅读并保存页码。缺少 Poppler 或页面渲染失败时，使用浏览器内置阅读器，此时只记录打开时间和阅读时长 |
| EPUB | 在网页中阅读，保存当前章节 |
| TXT、LOG、JSON、JSONL、YAML、TOML、INI、PROPERTIES | UTF-8 文本预览，保存滚动位置 |
| MD、MARKDOWN | 渲染 Markdown 标题、列表、表格和代码块，保存滚动位置 |
| CSV、TSV | 首行作为列名，每页显示 50 行数据，保存页码 |
| XLSX | 按工作表分页，每页 50 行，保存工作表和页码 |
| Parquet | 按行组读取，每页显示 50 行，保存页码 |
| DOCX | 预览正文段落和受支持的内嵌图片，保存滚动位置 |
| PPTX | 按幻灯片顺序预览文字，保存幻灯片页码 |
| PNG、JPEG、GIF、WebP、BMP、AVIF | 由浏览器预览图片 |

Office 预览以提取的内容为主。预览最多显示 100 列，文本类预览最多读取前 2 MiB。Office 文件的预览大小上限为 64 MiB，CSV、TSV 和 Parquet 为 512 MiB。超出预览范围的内容可通过阅读页下载原文件。

### 封面和元数据

WackyBooks 可读取 EPUB 内置元数据和封面。自定义封面可放在书籍同目录下，使用相同的主文件名和 `.jpg`、`.jpeg`、`.png` 或 `.webp` 后缀。

扫描时会保存书籍元数据。可用同名 JSON 补充或覆盖元数据，其他可选字段包括 `Description`、`Language`、`Identifier` 和 `Date`。同名且含有效元数据字段的 JSON 会作为附件。没有用作元数据或封面的 JSON 和图片可以作为独立条目阅读。下面是 `山间来信.epub` 对应的 `山间来信.json` 示例。

```json
{
  "Title": "山间来信",
  "Authors": ["林青"],
  "Publisher": "示例出版社",
  "Subjects": ["山地", "旅行"]
}
```

修改元数据后，在后台重新扫描书库。

## 登录与书库授权

| 登录名 | 认证方式 |
| --- | --- |
| `Admin` | 本地管理员 |
| 不含 `@` 的用户名 | 本地读者 |
| 含 `@` 的用户名 | LDAP / Active Directory，使用完整登录名 |

用户名是否含 `@` 决定认证方式。LDAP 账户必须使用包含 `@` 的完整登录名，本地账户名不能包含 `@`。

在「后台管理 → 用户管理」可以新增本地读者、重置本地密码、启用或停用账户。LDAP 用户首次成功登录后会出现在列表中。

在用户行点击「书库授权」，勾选允许访问的书库并保存。普通用户默认未授权，Admin 可访问全部书库。服务端会立即执行授权变更，读者刷新页面即可更新书库列表。书库改名保留授权，更换目录后需要重新授权。

## LDAP / Active Directory

用本地 Admin 进入「后台管理 → LDAP 配置」，填写下表中的配置项。

| 配置项 | 填写方式 |
| --- | --- |
| 服务器地址 | `ldaps://ldap.example.org:636` 使用 TLS。`ldap://ldap.example.org:389` 默认使用 StartTLS，勾选「明文 LDAP」可直接使用 LDAP。明文模式下，绑定密码和用户密码会在应用与 LDAP 服务器之间明文传输 |
| 用户搜索 Base DN | 例如 `ou=people,dc=example,dc=org` |
| 绑定账号和密码 | 填写可以搜索用户的服务账号。支持 DN 或目录接受的 UPN。目录允许匿名搜索时，两项均可留空 |
| 用户过滤器 | AD 使用完整 UPN 登录时，可填 `(userPrincipalName={username})`。OpenLDAP 按 UID 登录时，可填 `(uid={username})`。搜索必须匹配唯一用户 |
| 用户标识属性 | AD 常用 `objectGUID`，OpenLDAP 常用 `entryUUID`。目录无法提供可读的唯一属性时，可填 `dn` |
| CA 证书 | 使用私有 CA 时填写 PEM 证书。留空使用系统信任库 |

先点击「测试连接」验证服务账号绑定和 Base DN。也可以填写测试账号和密码，点击「测试登录」验证用户搜索和密码。通过后勾选「启用 LDAP 登录」并保存。

## OIDC / SSO

本地管理员登录「后台管理」，在「OIDC / SSO 配置」填写身份提供方信息。Casdoor 等支持 OpenID Connect Discovery 的服务可直接使用。

| 配置项 | 说明 |
| --- | --- |
| Issuer | 身份提供方声明的 issuer 地址，需与发现文档一致 |
| 回调地址 | 浏览器可访问的站点绝对地址，路径固定为 `/api/oidc/callback`。需要与身份提供方登记的 Redirect URI 完全一致 |
| Client ID / Client Secret | 在身份提供方创建的 OIDC 应用凭据 |
| Scopes | 空格分隔的 scope，留空时使用 `openid profile email` |
| 管理员 Claim / 值 | 可选。配置后，ID Token 中该 claim 匹配指定值的用户会获得管理员权限。未配置时，OIDC 用户是普通读者 |

先点「测试发现文档」验证 issuer 和端点，再保存并启用。登录页会显示单点登录入口。首次成功登录的 OIDC 用户会加入用户列表，默认没有书库授权。用户唯一标识由 issuer 和 subject 共同生成，因此同一身份提供方中的邮箱变更不会产生新账户。

登录使用 Authorization Code Flow，并校验 state、nonce、PKCE 和 ID Token。生产环境应使用 HTTPS，并在身份提供方将回调地址登记为精确匹配的 Redirect URI。

已注册用户更换标识方式，或在 DN 模式下更改该用户在目录中的 DN，会产生新的用户记录，需要重新授权。

## 运行配置与数据

| 环境变量 | 直接运行默认值 | Docker 默认值 | 用途 |
| --- | --- | --- | --- |
| `WACKY_ADDR` | `127.0.0.1:8080` | `0.0.0.0:8080` | HTTP 监听地址 |
| `WACKY_DATA` | `data` | `/data` | 状态文件所在的可写目录 |

### `WACKY_DATA` 文件说明

程序启动时会创建数据目录及 `state.sqlite`。应用使用纯 Go SQLite 驱动，继续支持 `CGO_ENABLED=0` 单文件构建。

| 文件 | 用途 |
| --- | --- |
| `state.sqlite` | 账户、书库授权、LDAP 配置、书籍元数据、阅读记录、收藏清单和收藏时间 |
| `state.sqlite-journal` | SQLite 事务期间可能生成的回滚日志 |

数据库文件权限为 `0600`，包含 LDAP 和 OIDC 客户端密钥。登录会话保存在内存中，服务重启后需重新登录。原始书籍、封面和同名元数据文件保存在各书库目录中。

### SQLite 表结构

当前数据库结构版本为 `4`（`PRAGMA user_version`）。首次启动时直接创建当前表结构。建表语句见 [app/storage_sqlite.go](app/storage_sqlite.go)。

| 表 | 整数主键与关联 | 用途 |
| --- | --- | --- |
| `settings` | 固定 `id=1` | 管理员密码哈希、LDAP 配置和 OIDC 客户端配置 |
| `libraries` | `id` | 书库名称、目录、排序和对外标识 |
| `readers` | `id` | 读者对外标识及收藏清单初始化状态 |
| `users` | `id` 关联 `readers.id` | 本地、LDAP 和 OIDC 账号 |
| `grants` | `id`，授权组合唯一 | 用户的书库访问授权 |
| `books` | `id`，`library_id` 关联书库 | 书籍元数据、搜索索引和扫描状态 |
| `reading` | `id`，读者与书籍组合唯一 | 阅读位置、进度、最近阅读时间和累计时长 |
| `lists` | `id`，同一读者的清单名称唯一 | 读者的收藏清单 |
| `favorites` | `id`，清单与书籍组合唯一 | 清单中的书籍、收藏时间和顺序 |

- `public_key` 是书库、读者和书籍的唯一文本标识。书库和书籍 API 使用整数 ID，书库的 public_key 用于保持扫描标识稳定。内置管理员的读者 `public_key` 使用空字符串。
- 历史书籍即使暂时无法扫描，也保留其收藏和阅读记录。书籍重新出现后会复用原有整数 ID。
- 阅读位置从 `0` 开始，表示页索引、章节索引或滚动像素位置。XLSX 位置同时编码工作表索引和页索引。
- `opened_ms` 和 `added_ms` 使用 Unix 毫秒时间戳。`duration_ms` 是累计时长，单位为毫秒。
- `books_library` 索引覆盖 `(library_id, active)`，`favorites_book` 索引覆盖 `(book_id, list_id)`。收藏人数查询对同一本书的 `reader_id` 去重，同一读者添加到多个清单时只计一人。

### 备份与恢复

停止服务后，备份 `WACKY_DATA` 目录和各书库目录。恢复时，将备份的数据目录和书库放回目标实例，保留读写权限。如果使用 Docker，恢复后的书库应挂载到数据库记录的容器内路径。

## 设计参考

- [早期需求和页面布局](docs/requirement.md)

## 许可证

本项目基于 [MIT License](LICENSE) 发布。
