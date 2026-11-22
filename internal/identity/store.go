package identity

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	"kubemono/internal/util/fsutil"
)

// NodeConfigFileName 是本机节点身份在 store 目录中的文件名。
// 文件名固定，Dir 只决定目录。
const NodeConfigFileName = "node.yaml"

// Store 读写本机节点身份（/usr/local/kubemono/node.yaml）。
// Dir 为空时使用 constants.KubemonoDir。写入使用 0600 + 同目录原子 rename，
// 目录 0700；读取与写入都拒绝最终路径上的符号链接，避免权限被绕开。
type Store struct {
	// Dir 是存放 node.yaml 的目录。
	Dir string
}

// NewStore 返回以 dir 为根的 store；dir 为空表示默认的 /usr/local/kubemono。
func NewStore(dir string) *Store { return &Store{Dir: dir} }

// dir 返回实际生效的目录。
func (s *Store) dir() string {
	if s == nil || s.Dir == "" {
		return constants.KubemonoDir
	}
	return s.Dir
}

// FilePath 返回节点身份文件的路径。
func (s *Store) FilePath() string {
	return filepath.Join(s.dir(), NodeConfigFileName)
}

// Load 读取并解码本机节点身份。
// 文件不存在时返回 (nil, nil)，调用方据此判断“尚未安装”。
func (s *Store) Load() (*clusterconfig.NodeConfig, error) {
	path := s.FilePath()
	if link, err := isSymlink(path); err != nil {
		return nil, fmt.Errorf("检查本机身份文件 %s 失败: %w", path, err)
	} else if link {
		return nil, fmt.Errorf("本机身份文件 %s 是符号链接，拒绝读取", path)
	}
	f, err := openNoFollow(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("打开本机身份文件 %s 失败: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("读取本机身份文件 %s 属性失败: %w", path, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("本机身份文件 %s 不是普通文件", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("读取本机身份文件 %s 失败: %w", path, err)
	}
	n, err := clusterconfig.LoadNodeConfig(data)
	if err != nil {
		return nil, fmt.Errorf("解析本机身份文件 %s 失败: %w", path, err)
	}
	return n, nil
}

// Save 原子地写入节点身份：目录 0700、文件 0600、临时文件 +
// rename，保证任何时刻读者看到的都是完整文件。
func (s *Store) Save(n *clusterconfig.NodeConfig) error {
	if n == nil {
		return fmt.Errorf("node config 为空，拒绝写入")
	}
	if strings.TrimSpace(n.NodeName) == "" {
		return fmt.Errorf("node config 缺少 nodeName，拒绝写入")
	}
	if n.HostName == "" {
		n.HostName = n.NodeName
	}
	dir := s.dir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建目录 %s 失败: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("设置目录 %s 权限失败: %w", dir, err)
	}
	di, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("检查目录 %s 失败: %w", dir, err)
	}
	if di.Mode()&fs.ModeSymlink != 0 || !di.IsDir() {
		return fmt.Errorf("%s 不是普通目录，拒绝写入本机身份", dir)
	}

	path := s.FilePath()
	if link, err := isSymlink(path); err != nil {
		return fmt.Errorf("检查本机身份文件 %s 失败: %w", path, err)
	} else if link {
		return fmt.Errorf("本机身份文件 %s 是符号链接，拒绝写入", path)
	}

	data, err := clusterconfig.MarshalNodeConfig(n)
	if err != nil {
		return fmt.Errorf("序列化本机身份失败: %w", err)
	}

	if err := fsutil.WriteFileAtomic(path, data, 0o600); err != nil {
		return fmt.Errorf("写入本机身份 %s 失败: %w", path, err)
	}
	return nil
}
