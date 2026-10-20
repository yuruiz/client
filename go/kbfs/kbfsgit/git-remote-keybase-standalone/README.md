# 独立 Keybase Git Remote Helper

复用本仓库官方 `kbfsgit`、`libgit`、`libkbfs`、身份验证引擎和团队密钥实现，
直接访问 Keybase API、KBFS metadata/block servers。无需安装或运行 `keybase service`、
`kbfsd`、FUSE，也不挂载文件系统。

现有 `keybase://private/<users>/<repo>`、`keybase://public/<user>/<repo>`、
`keybase://team/<team>/<repo>` URL 和远端 `.kbfs_git` 布局保持不变。
支持官方 Runner 的 list、fetch、push、clone、分支、标签以及 LFS custom transfer。

## 构建和使用

推荐在仓库根目录通过 Homebrew 安装：

```sh
bash packaging/homebrew/install-local.sh
git config --global keybase.username 你的用户名
git clone keybase://private/你的用户名/已有仓库
```

Homebrew 管理版本、安装位置和命令链接，不需要将 `/tmp` 加入 PATH。
已有设备状态和钥匙串条目在升级、重装和卸载时保留。详见
[Homebrew 打包说明](../../../../packaging/homebrew/README.md)。

用户名也可在单个仓库通过 `git config keybase.username 你的用户名` 设置；
显式 `--username` 和 `KEYBASE_GIT_USERNAME` 会覆盖 Git 配置。
已有本地仓库无需改 remote，继续使用普通 Git 命令。

开发时也可在 `go/` 目录用仓库指定工具链构建：

```sh
go build -o ./bin/git-remote-keybase ./kbfs/kbfsgit/git-remote-keybase-standalone
```

可执行文件必须叫 `git-remote-keybase`，Git 才会为原有 URL 选择它。

默认使用 `auto` 认证模式：若已显式配置过 Helper 专用设备，使用该设备；否则读取
这台机器上官方 Keybase 客户端已经保存的设备配置、
加密设备私钥以及 macOS 钥匙串中的解锁材料。无需输入 Paper Key，也不需要官方客户端
或 `keybase service` 正在运行。第一次读取钥匙串时，macOS 可能要求允许该二进制访问。

Helper 只读使用官方凭证：普通 Git 请求不注册新设备，不切换官方客户端的当前用户，不更新或删除
钥匙串条目、不打开官方客户端的缓存数据库。身份、签名链、团队审计等缓存保存在
Helper 自己的目录，后续请求继续按官方规则检查有效性，而不是每次从零审计。

可通过 `--keybase-home` / `KEYBASE_GIT_KEYBASE_HOME` 指定官方客户端使用的 home 根目录，
也遵循官方的 `KEYBASE_CONFIG_FILE` 和 `KEYBASE_SECRET_KEYRING_TEMPLATE` 路径配置。
`--cache-dir` / `KEYBASE_GIT_CACHE_DIR` 指定 Helper 缓存的父目录。默认缓存位于系统用户缓存目录
下的 `keybase-git-standalone/identities/`，按配置路径、账号和设备区分。

持久化设备认证当前支持 **macOS 原生钥匙串**。若设备已撤销、账号重置，或钥匙串无法
解锁，Helper 返回错误，不会悄悄改成 Paper Key 登录。可用 `--auth-mode existing` 强制
复用官方客户端凭证，或用 `--auth-mode device` 强制使用 Helper 专用设备。

### 旧凭证已丢失时：配置一次专用设备

```sh
git-remote-keybase --setup --username 你的用户名 --device-name "Git Remote Helper"
```

此命令使用一次 Paper Key 授权一个新设备；名称会显示在 Keybase 账号的公开设备列表中。
它沿用官方注册和密钥加密流程，在 Helper 自己的状态目录保存加密设备私钥，在独立的
macOS 钥匙串命名空间保存解锁材料，不保存 Paper Key。

配置完成后直接使用普通 Git 命令，无需再次提供 Paper Key。`--setup` 不执行 Git 推送，
也不会为已配置的 Helper 重复注册设备。普通 Git 请求不会触发设备注册。

默认状态目录是系统用户配置目录下的 `keybase-git-standalone/devices/<username>/`。
可用 `--state-dir` / `KEYBASE_GIT_STATE_DIR` 指定父目录。请保留状态目录和对应钥匙串条目；
钥匙串命名空间绑定规范化后的状态目录路径，改变路径会选择不同的身份存储。
专用设备的身份和审计缓存跨进程保留，并通过文件锁协调并发。

### 显式临时登录

未配置设备的环境仍可明确选择 `oneshot` 模式：

```sh
KEYBASE_GIT_AUTH_MODE=oneshot git clone keybase://private/你的用户名/已有仓库
```

该模式才会在实际访问仓库时，通过控制终端无回显询问 Paper Key。public 仓库也使用所选
认证模式。Git 的 stdin/stdout 始终只用于协议通信。不要把 Paper Key 放进命令参数。

无终端环境可为临时模式指定独立文件描述符：

```sh
KEYBASE_GIT_AUTH_MODE=oneshot KEYBASE_GIT_PAPER_KEY_FD=3 git fetch origin 3</安全位置/paper-key
```

文件由调用方保护，描述符至少为 3，读完关闭，长度上限为 4096 字节，以 EOF 结束。
一次 Git 命令若启动多个临时 Helper，每个进程都需要独立可读的密钥输入。
Helper 不保存 Paper Key。临时模式仍会丢弃身份缓存，适用于容器等一次性场景。

直接调用可使用 `--auth-mode auto|existing|device|oneshot`、`--username USER`；
`--paper-key-fd FD` 只允许与 `--setup` 或 `oneshot` 一起使用，所有参数须放在 remote 之前。
排查长时间无响应可加 `--diagnostics`：输出初始化耗时，实际请求超过 30 秒时向标准错误
输出协程调用栈（不输出函数参数值）。协议协商不会启动诊断计时器。

## 触发时机和生命周期

| 输入 | 行为 |
| --- | --- |
| `--help`、`--version` | 不建立会话 |
| `capabilities`、`option ...` | 本地应答，不初始化身份或 KBFS |
| LFS `init`、`terminate` | 本地应答 |
| 首个 `list`、`fetch`、`push` 或 LFS `upload`/`download` | 按所选模式解锁现有设备或临时登录，再初始化进程内 KBFS |
| 后续同进程请求 | 复用当前会话，按读取/写入需要执行官方签名、解密和加密 |
| 正常结束或可处理的中断 | 关闭连接和数据库，清除内存中的设备状态，删除临时 journal；持久化设备模式保留身份缓存 |

一个进程仅使用一个身份。持久化设备模式的状态和缓存目录权限为 `0700`，使用文件锁协调
同一设备的并发请求，锁在退出时释放。进程意外退出后，后续调用仍可重新取得锁；
Helper 不运行常驻服务。同一设备同时进行多个 Git 操作时会依次访问这份缓存。

KBFS journal 和操作临时目录每次独立创建，正常结束时删除。SIGKILL、掉电等无法执行
清理的情形可能留下临时目录。首次建立身份缓存仍需执行完整的官方历史审计，耗时可能
超过两分钟；保留缓存后可由官方审计逻辑复用已经验证的结果，不跳过撤销检查或安全校验。

仍保留官方 Merkle/sigchain 校验、团队成员权限检查、签名和 KBFS 密文格式。
身份校验需要接受异常证明时返回错误，不自动接受。所使用的设备或 Paper Key 必须未撤销，
且具有目标仓库密钥访问权限；旧 private TLF 若尚未向该设备分发密钥，
仍需要有权限的现有设备完成 rekey。

该 Helper 不运行聊天服务，不发送团队推送聊天通知；Git 元数据仍按官方方式发布。
认证和远端读写需要网络。“独立”指不依赖本机服务，并非离线访问 Keybase 服务器。
Git 工作树和 `.git` 中的本地副本仍是普通 Git 数据；远端存储沿用 KBFS 加密。

## 验证

无需账户或服务即可运行的回归测试：

```sh
go test ./kbfs/kbfsgit -run TestOnDemand
go test ./kbfs/kbfsgit/git-remote-keybase-standalone
go test ./service -run TestKBFSInProcess
go test ./git -run TestSendChatWithoutChatRuntime
```

可选的线上公开身份加载检查（无需 Paper Key，不登录）：

```sh
KEYBASE_GIT_LIVE_TEST_USERNAME=你的用户名 \
  go test ./kbfs/kbfsgit/git-remote-keybase-standalone -run '^TestLivePublicIdentityLookup$'
```

覆盖协商零认证、错误和清理、描述符输入、官方配置与密钥文件保持不变、
跨进程设备解锁、审计缓存保留、并发缓存锁、撤销/解锁失败拒绝、本地 socket 拒绝、
进程内签名/解密及密文篡改拒绝，并用官方内存 KBFS 测试服务器进行双向互操作：
新入口推送 → 官方 Runner 读取，官方 Runner 更新 → 新入口克隆。
另外已在 macOS 完成生产账户的一次性设备配置、钥匙串凭证跨进程恢复和 private 仓库的
两次隔离 fetch 验证；第二次运行复用了持久化身份缓存，无需 Paper Key。
没有对真实仓库执行推送；生产环境写入和其他账号/团队权限场景仍需独立验收。
