package appstore

import (
	"testing"
	"time"
)

func TestValidateIndexURL(t *testing.T) {
	valid := []string{
		"https://raw.githubusercontent.com/YUNXI-fi/Panel-SSH/main/apps/index.json",
		"http://192.168.1.10:8080/apps/index.json",
	}
	for _, value := range valid {
		if err := ValidateIndexURL(value); err != nil {
			t.Fatalf("%s 应被接受：%v", value, err)
		}
	}

	invalid := []string{
		"",
		"file:///etc/passwd.json",
		"ftp://example.com/apps/index.json",
		"https://example.com/apps/",
		"https:///apps/index.json",
		"index.json",
	}
	for _, value := range invalid {
		if err := ValidateIndexURL(value); err == nil {
			t.Fatalf("%q 应被拒绝", value)
		}
	}
}

// 切换仓库源必须丢掉旧索引：否则界面会继续展示旧源的包与版本。
func TestSetIndexURLDropsCachedIndex(t *testing.T) {
	store := NewStore(t.TempDir(), "")
	first := "https://example.com/a/index.json"
	second := "https://example.com/b/index.json"

	store.SetIndexURL(first)
	store.mu.Lock()
	store.index = &Index{SchemaVersion: SchemaVersion, Apps: []Entry{{Slug: "demo"}}}
	store.indexAt = time.Now()
	store.mu.Unlock()

	store.SetIndexURL(second)

	if got := store.IndexURL(); got != second {
		t.Fatalf("地址没有切换：%s", got)
	}
	store.mu.Lock()
	cached := store.index
	store.mu.Unlock()
	if cached != nil {
		t.Fatal("切换源后旧索引仍留在内存里")
	}
}
