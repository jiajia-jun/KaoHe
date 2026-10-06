// Package storage 负责原始文件在持久化目录中的落盘与读取。
//
// 目录布局：<root>/<documentUID>/original.<ext>
//
// 存储键与用户看到的文件名完全分离，因此：
//   - 同名文件多次上传不会互相覆盖；
//   - 用户在界面上重命名文件不会移动磁盘上的字节；
//   - 下载时的文件名由数据库记录生成，不接受客户端传入任何路径。
package storage

import (
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

type Store struct {
	root string
}

func New(root string) (*Store, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("解析存储根目录失败: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("创建存储根目录 %s 失败: %w", abs, err)
	}
	return &Store{root: abs}, nil
}

// Root 返回存储根目录的绝对路径。
func (s *Store) Root() string { return s.root }

// Save 把 r 的内容写入文档专属目录。
//
// 先写临时文件、成功后再 rename：中途失败（网络中断、磁盘写满、进程被杀）
// 不会在持久化目录里留下半个文件冒充完整文件。
func (s *Store) Save(docUID, ext string, r io.Reader) (storageKey string, size int64, err error) {
	dir, err := s.docDir(docUID)
	if err != nil {
		return "", 0, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", 0, fmt.Errorf("创建文档目录失败: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return "", 0, fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()

	// 任何失败路径都要清理临时文件，避免在持久化目录里堆积垃圾
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}

	size, err = io.Copy(tmp, r)
	if err != nil {
		cleanup()
		return "", 0, fmt.Errorf("写入文件失败: %w", err)
	}
	// 显式 Sync 再 rename，保证 rename 后文件内容确实落盘
	if err := tmp.Sync(); err != nil {
		cleanup()
		return "", 0, fmt.Errorf("刷新文件到磁盘失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return "", 0, fmt.Errorf("关闭临时文件失败: %w", err)
	}

	storageKey = path.Join(docUID, "original"+ext)
	if err := os.Rename(tmpName, filepath.Join(s.root, filepath.FromSlash(storageKey))); err != nil {
		os.Remove(tmpName)
		return "", 0, fmt.Errorf("落盘失败: %w", err)
	}
	return storageKey, size, nil
}

// Open 打开已保存的文件，供下载接口流式返回。
func (s *Store) Open(storageKey string) (*os.File, os.FileInfo, error) {
	full, err := s.resolve(storageKey)
	if err != nil {
		return nil, nil, err
	}
	f, err := os.Open(full)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, info, nil
}

// RemoveMany 删除文档占用的整个目录。用于上传中途失败时的回滚。
func (s *Store) RemoveMany(docUID string) error {
	dir, err := s.docDir(docUID)
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

// Exists 判断存储键对应的文件是否仍然存在。
// 详情页用它区分“索引失败”与“原文件已丢失”两种不同原因。
func (s *Store) Exists(storageKey string) bool {
	full, err := s.resolve(storageKey)
	if err != nil {
		return false
	}
	info, err := os.Stat(full)
	return err == nil && info.Mode().IsRegular()
}

func (s *Store) docDir(docUID string) (string, error) {
	if err := validateUID(docUID); err != nil {
		return "", err
	}
	return filepath.Join(s.root, docUID), nil
}

// resolve 把存储键解析成绝对路径，并确认它没有逃出存储根目录。
// 存储键虽然由服务端生成，仍做一次越界校验，避免将来引入拼接错误。
func (s *Store) resolve(storageKey string) (string, error) {
	if storageKey == "" {
		return "", fmt.Errorf("存储键为空")
	}
	clean := filepath.Clean(filepath.FromSlash(storageKey))
	if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
		return "", fmt.Errorf("非法存储键: %q", storageKey)
	}
	full := filepath.Join(s.root, clean)
	if full != s.root && !strings.HasPrefix(full, s.root+string(os.PathSeparator)) {
		return "", fmt.Errorf("存储键越界: %q", storageKey)
	}
	return full, nil
}

func validateUID(uid string) error {
	if uid == "" || len(uid) > 64 {
		return fmt.Errorf("非法文档标识: %q", uid)
	}
	for _, r := range uid {
		if !(r == '_' || r == '-' ||
			(r >= '0' && r <= '9') ||
			(r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z')) {
			return fmt.Errorf("非法文档标识: %q", uid)
		}
	}
	return nil
}
