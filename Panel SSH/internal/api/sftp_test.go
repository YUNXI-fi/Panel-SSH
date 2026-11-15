package api

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// withShortOpTimeout 把元数据操作的超时压到毫秒级，用完还原。
// 不这样做的话，验证"超时确实会生效"这条断言本身就要跑满 20 秒。
func withShortOpTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := sftpOpTimeout
	sftpOpTimeout = d
	t.Cleanup(func() { sftpOpTimeout = old })
}

func TestCallTimesOutWhenRemoteStalls(t *testing.T) {
	withShortOpTimeout(t, 30*time.Millisecond)

	guard := &sftpGuard{}
	start := time.Now()
	_, err := call(guard, func() (string, error) {
		// 模拟远端不再回应：调用永久阻塞，只有连接被关闭才会返回。
		time.Sleep(2 * time.Second)
		return "late", nil
	})

	if !errors.Is(err, errSFTPTimeout) {
		t.Fatalf("期望超时错误，实际得到 %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("超时未被及时触发，耗时 %v", elapsed)
	}
}

func TestCallReturnsResultOnSuccess(t *testing.T) {
	withShortOpTimeout(t, time.Second)

	value, err := call(&sftpGuard{}, func() (int, error) { return 42, nil })
	if err != nil {
		t.Fatalf("不该报错：%v", err)
	}
	if value != 42 {
		t.Fatalf("期望 42，实际 %d", value)
	}
}

func TestCallPropagatesError(t *testing.T) {
	withShortOpTimeout(t, time.Second)

	sentinel := errors.New("远端拒绝")
	_, err := call(&sftpGuard{}, func() (int, error) { return 0, sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("期望透传原始错误，实际 %v", err)
	}
}

// 传输停滞判定：只要还有字节流动就不能误杀，一旦停止流动就必须中止。
func TestStallGuardFiresOnlyAfterSilence(t *testing.T) {
	var closed atomic.Int32
	guard := newStallGuard(80*time.Millisecond, func() { closed.Add(1) })
	defer guard.Release()

	// 持续有进展：不应触发。
	deadline := time.Now().Add(240 * time.Millisecond)
	for time.Now().Before(deadline) {
		guard.touch()
		time.Sleep(10 * time.Millisecond)
	}
	if closed.Load() != 0 {
		t.Fatalf("持续有进展时不应中断，实际触发 %d 次", closed.Load())
	}

	// 停止进展：应当触发一次。
	time.Sleep(300 * time.Millisecond)
	if closed.Load() != 1 {
		t.Fatalf("停滞后期望触发一次中断，实际 %d 次", closed.Load())
	}
}

func TestStallGuardReleaseStopsWatching(t *testing.T) {
	var closed atomic.Int32
	guard := newStallGuard(30*time.Millisecond, func() { closed.Add(1) })
	guard.Release()

	time.Sleep(150 * time.Millisecond)
	if closed.Load() != 0 {
		t.Fatalf("Release 之后不应再触发，实际 %d 次", closed.Load())
	}
}

// 进度包装器：有字节流动就必须刷新计时，否则长传输会被误判为停滞。
func TestProgressWrappersTouchOnData(t *testing.T) {
	guard := newStallGuard(time.Hour, func() {})
	defer guard.Release()

	reader := progressReader{r: strings.NewReader("panel-ssh"), g: guard}
	before := guard.last
	time.Sleep(5 * time.Millisecond)
	if _, err := reader.Read(make([]byte, 4)); err != nil {
		t.Fatalf("读取不应失败：%v", err)
	}
	if !guard.last.After(before) {
		t.Fatal("读侧有数据时未刷新进度计时")
	}

	var sink strings.Builder
	writer := progressWriter{w: &sink, g: guard}
	before = guard.last
	time.Sleep(5 * time.Millisecond)
	if _, err := writer.Write([]byte("x")); err != nil {
		t.Fatalf("写入不应失败：%v", err)
	}
	if !guard.last.After(before) {
		t.Fatal("写侧有数据时未刷新进度计时")
	}
	if sink.String() != "x" {
		t.Fatalf("写入内容不符：%q", sink.String())
	}
}
