package api

import (
	"log"
	"os"
	"time"

	"panelssh/internal/appstore"
)

// packageApp 把磁盘上的应用包转换成通用窗口使用的 securityApp。
//
// 应用包可以只声明命令模板，也可以用 builtin 标记复用面板编译期实现的
// 动作（GnuPG 的密钥生成需要临时文件与多步编排，不适合写成一条命令）。
// 未安装同名应用包时返回 nil，调用方回退到内置安全应用目录。
func (s *Server) packageApp(slug string) *securityApp {
	if s.appPackages == nil {
		return nil
	}
	manifest, ui, err := s.appPackages.Installed(slug)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("应用包 %s 读取失败，回退到内置目录：%v", slug, err)
		}
		return nil
	}

	actions := make([]*securityAction, 0, len(manifest.Actions))
	var builtin *securityApp
	for _, spec := range manifest.Actions {
		if spec.Builtin {
			if builtin == nil {
				builtin = securityApps[slug]
			}
			if builtin == nil {
				continue
			}
			for _, action := range builtin.Actions {
				if action.ID == spec.ID {
					actions = append(actions, action)
					break
				}
			}
			continue
		}
		actions = append(actions, packageAction(spec))
	}
	if len(actions) == 0 {
		return nil
	}

	config := make([]securityConfigEntry, 0, len(manifest.Config))
	for _, item := range manifest.Config {
		config = append(config, securityConfigEntry{Path: item.Path, IsDir: item.IsDir})
	}

	defaultAction := manifest.DefaultAction
	if defaultAction == "" {
		defaultAction = actions[0].ID
	}
	return &securityApp{
		Slug:          manifest.Slug,
		Name:          manifest.Name,
		Description:   manifest.Description,
		DefaultAction: defaultAction,
		Actions:       actions,
		Config:        config,
		Version:       manifest.Version,
		Category:      manifest.Category,
		IconURL:       "/api/store/apps/" + manifest.Slug + "/icon",
		UI:            ui,
	}
}

func packageAction(spec appstore.Action) *securityAction {
	fields := make([]*securityField, 0, len(spec.Fields))
	for _, field := range spec.Fields {
		kind := field.Type
		if kind == "" {
			kind = "text"
		}
		maxLen := field.MaxLen
		if maxLen <= 0 {
			maxLen = 200
		}
		fields = append(fields, &securityField{
			Key:         field.Key,
			Label:       field.Label,
			Type:        kind,
			Placeholder: field.Placeholder,
			Default:     field.Default,
			Options:     field.Options,
			Required:    field.Required,
			Help:        field.Help,
			pattern:     field.Pattern,
			maxLen:      maxLen,
		})
	}

	group := spec.Group
	if group == "" {
		group = "inspect"
	}
	if spec.Danger {
		group = "manage"
	}
	timeout := time.Duration(spec.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	command := spec.Command
	return &securityAction{
		ID:          spec.ID,
		Label:       spec.Label,
		Description: spec.Description,
		Group:       group,
		Fields:      fields,
		Confirm:     spec.Confirm,
		Danger:      spec.Danger,
		needRoot:    spec.NeedRoot,
		timeout:     timeout,
		build: func(params map[string]string) (string, error) {
			return appstore.Render(command, params), nil
		},
	}
}

// PackageActions 返回内置安全应用的动作元数据，供应用包生成器写入 app.json。
//
// 只导出元数据，不导出 build：远程包用 Builtin 标记复用编译期实现，
// 这样 GnuPG 这类需要多步编排的动作不会被降级成一条命令模板。
func PackageActions(slug string) []appstore.Action {
	app := securityApps[slug]
	if app == nil {
		return nil
	}
	out := make([]appstore.Action, 0, len(app.Actions))
	for _, action := range app.Actions {
		fields := make([]appstore.Field, 0, len(action.Fields))
		for _, field := range action.Fields {
			fields = append(fields, appstore.Field{
				Key: field.Key, Label: field.Label, Type: field.Type,
				Placeholder: field.Placeholder, Default: field.Default,
				Options: field.Options, Required: field.Required, Help: field.Help,
				Pattern: field.pattern, MaxLen: field.maxLen,
			})
		}
		out = append(out, appstore.Action{
			ID: action.ID, Label: action.Label, Description: action.Description,
			Group: action.Group, Fields: fields, Confirm: action.Confirm,
			Danger: action.Danger, NeedRoot: action.needRoot,
			TimeoutSec: int(action.timeout / time.Second), Builtin: true,
		})
	}
	return out
}

// PackageDefaultAction 返回内置安全应用声明的默认动作，供生成器保持
// 应用包与现有窗口"打开即执行哪一项"的行为一致。
func PackageDefaultAction(slug string) string {
	if app := securityApps[slug]; app != nil {
		return app.DefaultAction
	}
	return ""
}
