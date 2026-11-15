package env

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// CatalogItemStatus 是软件目录在某台目标机上的当前状态。
type CatalogItemStatus struct {
	Slug         string          `json:"slug"`
	Name         string          `json:"name"`
	Category     string          `json:"category"`
	Description  string          `json:"description"`
	UseCase      string          `json:"useCase,omitempty"`
	Launch       *LaunchSpec     `json:"launch,omitempty"`
	Supported    bool            `json:"supported"`
	Installed    bool            `json:"installed"`
	Version      string          `json:"version"`
	Versions     []VersionOption `json:"versions,omitempty"`
	VersionError string          `json:"versionError,omitempty"`
	Note         string          `json:"note,omitempty"`
}

// CatalogStatus 一次性返回整个软件目录在目标机上的状态。
//
// 所有条目的版本探测合并进一段脚本、一次 SSH 往返完成；版本候选的获取
// （仅在线拉取的那些条目）与远程探测并行，互不拖慢。
func CatalogStatus(ctx context.Context, ex Executor, p *Profile) ([]CatalogItemStatus, error) {
	var script strings.Builder
	script.WriteString("export LC_ALL=C\n")
	for i, r := range catalog {
		fmt.Fprintf(&script, "echo ===%d===\n", i)
		if r.Supports(p.Family) {
			// 探测命令自身带 2>/dev/null；这里包一层防止命令不存在时中断脚本。
			script.WriteString("(" + r.DetectCommand(p) + ") 2>/dev/null | head -1\n")
		}
	}
	script.WriteString("echo ===END===\n")

	res, err := ex.Run(ctx, Wrap(script.String(), nil), queryTimeout)
	if err != nil {
		return nil, err
	}
	sections := splitSections(res.Stdout)

	items := make([]CatalogItemStatus, len(catalog))
	var wg sync.WaitGroup
	for i, r := range catalog {
		supported := r.Supports(p.Family)
		version := strings.TrimSpace(sections[fmt.Sprintf("%d", i)])

		items[i] = CatalogItemStatus{
			Slug: r.Slug, Name: r.Name, Category: r.Category, Description: r.Description,
			UseCase: r.UseCase, Note: r.Note, Launch: r.Launch,
			Supported: supported, Installed: supported && version != "", Version: version,
		}

		// 版本候选只有受支持且声明了 Versions 的目录项才取；
		// Go 的在线列表与远程探测并行，失败只影响下拉选项不影响安装表单。
		if supported && r.Versions != nil {
			wg.Add(1)
			go func(idx int, recipe *Recipe) {
				defer wg.Done()
				opts, verr := recipe.Versions(ctx, p)
				if verr != nil {
					items[idx].VersionError = "在线版本列表获取失败，可手动输入版本号"
					return
				}
				items[idx].Versions = opts
			}(i, r)
		}
	}
	wg.Wait()
	return items, nil
}
