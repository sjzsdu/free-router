package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/sjzsdu/free-router/internal/provider"
	"github.com/sjzsdu/free-router/internal/routing"
	"github.com/spf13/cobra"
)

type documentedRoute struct {
	Comment     string   `json:"_comment"`
	Type        string   `json:"type"`
	RequireTool bool     `json:"require_tool,omitempty"`
	Models      []string `json:"models"`
}

type documentedConfig struct {
	Comment     string                           `json:"_comment"`
	Help        map[string]string                `json:"_help"`
	Version     int                              `json:"version"`
	ProviderEnv map[string][]string              `json:"provider_env"`
	Routes      map[string]documentedRoute       `json:"routes"`
	Models      map[string]routing.ModelOverride `json:"models"`
}

var routeDescriptions = map[string]string{
	"chat":       "通用文本对话。models 按从高到低排列 fallback 优先级。",
	"chat-tools": "需要 tool/function call 的对话；候选模型必须支持工具调用。",
	"embedding":  "文本向量与语义检索模型。",
	"audio":      "语音合成、转录或翻译模型。",
	"image":      "图像生成与图像处理模型。",
	"video":      "视频生成与处理模型。",
	"rerank":     "检索结果重排序模型。",
	"moderation": "内容审核与安全分类模型。",
}

func addOnboardCommand(root *cobra.Command, opts *options) {
	force := false
	stdout := false
	command := &cobra.Command{
		Use:   "onboard [path]",
		Short: "Generate a documented configuration from built-in defaults",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			if stdout && (force || len(args) > 0) {
				return errors.New("--stdout cannot be combined with a path or --force")
			}
			content, err := documentedDefaultConfig()
			if err != nil {
				return err
			}
			if stdout {
				_, err = command.OutOrStdout().Write(content)
				return err
			}
			path := opts.config
			if len(args) == 1 {
				path = args[0]
			}
			if err := writeOnboardConfig(path, content, force); err != nil {
				return err
			}
			fmt.Fprintf(command.OutOrStdout(), "Created documented configuration: %s\n", path)
			fmt.Fprintf(command.OutOrStdout(), "Included %d built-in provider mappings and %d route types.\n", len(provider.DefaultEnvMap()), len(routing.DefaultConfig().Routes))
			fmt.Fprintln(command.OutOrStdout(), "Next: add a provider credential, then run free-router serve or free-router daemon install.")
			return nil
		},
	}
	command.Flags().BoolVarP(&force, "force", "f", false, "replace an existing configuration file")
	command.Flags().BoolVar(&stdout, "stdout", false, "print the generated configuration without writing a file")
	root.AddCommand(command)
}

func documentedDefaultConfig() ([]byte, error) {
	defaults := routing.DefaultConfig()
	routes := make(map[string]documentedRoute, len(defaults.Routes))
	for alias, route := range defaults.Routes {
		description := routeDescriptions[alias]
		if description == "" {
			description = fmt.Sprintf("%s 类型的能力路由。", route.Type)
		}
		routes[alias] = documentedRoute{
			Comment: description, Type: route.Type,
			RequireTool: route.RequireTool, Models: append([]string{}, route.Models...),
		}
	}
	config := documentedConfig{
		Comment: "由 free-router onboard 根据当前程序内置默认值生成。以 _ 开头的字段仅用于说明，不参与运行。API Key 不要写入此文件。",
		Help: map[string]string{
			"version":      "配置格式版本，由程序维护；不要手工降级。",
			"provider_env": "Provider 到 API Key 环境变量名数组的映射，按顺序使用第一个非空变量。这里只写变量名，不写 Key；用户映射会与内置映射合并。Cloudflare 还需要 CLOUDFLARE_ACCOUNT_ID。",
			"routes":       "稳定的功能路由名。每个 models 数组按从高到低排列；依次失败后，路由器会从该类型剩余的健康模型中选择。空数组表示完全自动选择。",
			"models":       "按 provider/model ID 覆盖上游元数据。可设置 disabled、type、tool_call、vision、reasoning；默认没有覆盖。",
		},
		Version: defaults.Version, ProviderEnv: provider.DefaultEnvMap(), Routes: routes, Models: defaults.Models,
	}
	content, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode default configuration: %w", err)
	}
	return append(content, '\n'), nil
}

func writeOnboardConfig(path string, content []byte, force bool) error {
	path = filepath.Clean(path)
	if path == "." || path == "" {
		return errors.New("configuration path must not be empty")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create configuration directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("secure configuration directory: %w", err)
	}
	if !force {
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("configuration already exists at %s; use --force to replace it", path)
		}
		if err != nil {
			return fmt.Errorf("create configuration: %w", err)
		}
		if _, err := file.Write(content); err != nil {
			file.Close()
			_ = os.Remove(path)
			return fmt.Errorf("write configuration: %w", err)
		}
		if err := file.Close(); err != nil {
			_ = os.Remove(path)
			return fmt.Errorf("close configuration: %w", err)
		}
		return nil
	}

	temporary, err := os.CreateTemp(dir, ".onboard-*")
	if err != nil {
		return fmt.Errorf("create temporary configuration: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(content); err != nil {
		temporary.Close()
		return fmt.Errorf("write configuration: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close configuration: %w", err)
	}
	if runtime.GOOS == "windows" {
		_ = os.Remove(path)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace configuration: %w", err)
	}
	return os.Chmod(path, 0o600)
}
