package env

import "errors"

// 环境管理操作的领域错误。
var (
	// ErrInvalidPackage 表示包名或版本含非法字符。
	ErrInvalidPackage = errors.New("包名或版本包含非法字符")
	// ErrNoSpecs 表示没有指定任何操作目标。
	ErrNoSpecs = errors.New("没有指定任何软件包")
	// ErrNoPrivilege 表示目标机没有任何可用的提权路径。
	ErrNoPrivilege = errors.New("需要 root 权限：该主机既非 root 登录，也没有可用的提权路径（su 认的是主机上另配的提权口令，sudo 认的是执行者自己的口令 —— 免密或投喂登录凭据都算）")
	// ErrVersionUnavailable 表示 apk 等家族不支持选择历史版本。
	ErrVersionUnavailable = errors.New("该包管理器不支持选择历史版本，只能安装仓库当前版本")
	// ErrUnknownMirror 表示请求了不支持的镜像站标识。
	ErrUnknownMirror = errors.New("未知镜像站")
)
