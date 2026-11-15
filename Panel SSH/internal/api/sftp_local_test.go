package api

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"panelssh/internal/store"
)

// 宿主机文件管理建立在进程内 SFTP 上，这条测试守住三件事：
// 会话能建起来、路径解析到真实文件系统、读写与远端走同一套调用。
func TestLocalSFTPListReadWrite(t *testing.T) {
	sess, guard, err := newLocalSFTP()
	if err != nil {
		t.Fatalf("建立本地 SFTP 会话失败：%v", err)
	}
	defer sess.Close()

	dir := t.TempDir()
	target := filepath.Join(dir, "note.txt")

	file, err := guard.Create(target)
	if err != nil {
		t.Fatalf("创建文件失败：%v", err)
	}
	if _, err := file.Write([]byte("hello")); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("关闭文件失败：%v", err)
	}

	info, err := guard.Stat(target)
	if err != nil {
		t.Fatalf("Stat 失败：%v", err)
	}
	if info.IsDir() || info.Size() != 5 {
		t.Fatalf("Stat 结果不符：dir=%v size=%d", info.IsDir(), info.Size())
	}

	items, err := guard.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir 失败：%v", err)
	}
	if len(items) != 1 || items[0].Name() != "note.txt" {
		t.Fatalf("目录内容不符：%v", items)
	}

	opened, err := guard.Open(target)
	if err != nil {
		t.Fatalf("打开文件失败：%v", err)
	}
	data, err := io.ReadAll(opened)
	_ = opened.Close()
	if err != nil {
		t.Fatalf("读取失败：%v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("内容不符：%q", data)
	}
}

// 宿主机作为传输目标时没有可 ssh 的地址，必须直接判定为"不能直连"，
// 否则会拼出一条 ssh -p 0 @ 的命令，白白探测一次才回退到中转。
func TestCanDirectTransferRejectsLocalTarget(t *testing.T) {
	s := &Server{}
	if s.canDirectTransfer(context.Background(), 1, &store.Host{ID: LocalHostID}) {
		t.Fatal("目标是宿主机时不应尝试直连")
	}
}

// 会话关闭后内置服务端必须退出，不能把管道和 goroutine 留在进程里。
func TestLocalSFTPCloseReleasesServer(t *testing.T) {
	sess, guard, err := newLocalSFTP()
	if err != nil {
		t.Fatalf("建立本地 SFTP 会话失败：%v", err)
	}
	sess.Close()

	if _, err := guard.Stat(os.TempDir()); err == nil {
		t.Fatal("会话关闭后仍能访问文件系统，说明连接没有真正断掉")
	}
}
