# Homebrew 安装与发布

这是基于 Keybase 官方代码的独立远程助手，包名为 `git-remote-keybase-standalone`，
安装的命令名为 `git-remote-keybase`。当前支持 macOS。

源码位于 [yuruiz/client 的 standalone-git-helper 分支](https://github.com/yuruiz/client/tree/standalone-git-helper)。
本机安装使用本地 tap；GitHub fork 本身尚未提供远端 Homebrew tap 或下载 release。

## 在本机通过 Homebrew 安装

在此仓库根目录运行：

```sh
bash packaging/homebrew/install-local.sh
```

脚本生成带固定版本和 SHA-256 校验值的源码归档，建立本机
`local/keybase-git` tap，再由 Homebrew 构建、安装并执行包测试。无需手动复制二进制，
也不需要把 `/tmp` 加入 PATH。首次构建会通过 Homebrew 安装 Go。
脚本不发布 GitHub 仓库、不上传源码、不注册设备。

安装后：

```sh
brew info local/keybase-git/git-remote-keybase-standalone
git-remote-keybase --version
git config --global keybase.username 你的用户名
git fetch
```

如果不同仓库使用不同账号，在对应仓库执行 `git config keybase.username 你的用户名`。
`--username`、`KEYBASE_GIT_USERNAME` 的优先级高于 Git 配置。

程序由 Homebrew 安装到：

- Apple Silicon：`/opt/homebrew/Cellar/git-remote-keybase-standalone/<版本>/`
- Intel：`/usr/local/Cellar/git-remote-keybase-standalone/<版本>/`

Homebrew 将命令链接到其 `bin` 目录，并提供稳定的 `opt` 链接。
如果已有同名官方客户端命令占用同一位置，Homebrew 会报告链接冲突；脚本不会强制覆盖它。

## 升级、重装和卸载

```sh
brew upgrade local/keybase-git/git-remote-keybase-standalone
brew reinstall local/keybase-git/git-remote-keybase-standalone
brew uninstall local/keybase-git/git-remote-keybase-standalone
```

本机 tap 的更新来源是此源码仓库。代码变更后，提高 `packaging/homebrew/VERSION`，
再运行安装脚本；也可以传入新版本号：

```sh
bash packaging/homebrew/install-local.sh 0.1.2
```

同一版本的源码归档不可覆盖成不同内容，避免已经安装的版本与来源不一致。
使用本机 tap 时，`brew update` 不会从尚未发布的远端自动拉取此项目的新版本。

设备状态、加密私钥、身份缓存和系统钥匙串条目均位于 Cellar 之外，安装脚本不修改它们。
升级、重装或卸载不会删除或撤销已配置设备。macOS 可能要求允许新二进制访问钥匙串，
但这不要求再次输入 Paper Key，也不要求注册新设备。

源码归档默认保存在 `~/.local/share/keybase-git-standalone/releases/`，而非 `/tmp`。
可通过 `KEYBASE_GIT_RELEASE_DIR` 修改归档目录，通过 `KEYBASE_GIT_HOMEBREW_TAP`
修改 tap 名称。归档和已生成 Formula 的文件 URL 必须保留，才能在本机重装。

## 生成可发布的源码包

```sh
python3 packaging/homebrew/release.py --version 0.1.1 --output /持久位置/releases
```

输出源码归档、校验文件和 Formula。源码快照包括当前 Go 工作区的已跟踪文件和未忽略的新文件，
因此未提交的 Helper 实现也会被包含；不会从上游仓库下载一个不含这些改动的版本。
`SOURCE.json` 记录基准提交、是否有工作区改动及文件摘要。
脚本排除 Git 忽略的文件，拒绝越出源码目录的符号链接。

公开发布前，应提交并审查源码，将归档上传到你控制的固定 HTTPS 下载地址，再用
`--source-url` 和 `--homepage` 生成指向该发布地址的 Formula：

```sh
python3 packaging/homebrew/release.py --version 0.1.1 \
  --output /持久位置/releases \
  --source-url https://你的发布地址/git-remote-keybase-standalone-0.1.1.tar.gz \
  --homepage https://你的项目主页
```

将 Formula 放入自己的 `homebrew-<名称>` 仓库的 `Formula/` 下发布。之后其他机器可使用
`brew tap <所有者>/<名称>` 和 `brew install <所有者>/<名称>/git-remote-keybase-standalone`。
此处的地址是说明格式，不代表已经创建或发布了远端仓库。

## 预编译二进制包

本机安装脚本使用 `--build-bottle`，可继续生成 Homebrew 原生 bottle：

```sh
mkdir -p "$HOME/.local/share/keybase-git-standalone/bottles"
cd "$HOME/.local/share/keybase-git-standalone/bottles"
brew bottle --json local/keybase-git/git-remote-keybase-standalone
```

公开分发时为目标架构/系统分别构建并上传 bottle，再将对应的地址和校验值合入 Formula。
没有匹配 bottle 的机器仍可从源码构建。此流程无需额外的后台服务或安装后登录钩子。

## 检查

```sh
python3 -m unittest discover -s packaging/homebrew -p 'test_*.py'
brew test local/keybase-git/git-remote-keybase-standalone
brew style local/keybase-git/git-remote-keybase-standalone
```

Formula 测试版本输出和远程助手协议协商，并确认测试不创建身份状态或缓存。
它不索取真实账号凭证，不访问真实仓库。真实仓库验证应另行执行。
