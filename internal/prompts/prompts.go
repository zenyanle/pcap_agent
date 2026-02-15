package prompts

import (
	"embed"
	"errors"
	"io/fs"
	"path/filepath"
)

//go:embed *.md
var promptFiles embed.FS

// GetPrompts 返回所有 prompt 文件的 map，key 为文件名（不含扩展名），value 为文件内容
func GetPrompts() (map[string]string, error) {
	prompts := make(map[string]string)

	err := fs.WalkDir(promptFiles, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// 跳过目录，只处理 .md 文件
		if d.IsDir() || filepath.Ext(path) != ".md" {
			return nil
		}

		// 读取文件内容
		content, err := promptFiles.ReadFile(path)
		if err != nil {
			return err
		}

		// 使用不带扩展名的文件名作为 key
		fileName := filepath.Base(path)
		key := fileName[:len(fileName)-len(filepath.Ext(fileName))]
		prompts[key] = string(content)

		return nil
	})

	if err != nil {
		return nil, err
	}

	return prompts, nil
}

func GetSinglePrompt(name string) (val string, err error) {
	prompts, err := GetPrompts()
	if err != nil {
		return "", err
	}
	val, ok := prompts[name]
	if !ok {
		return "", errors.New("the prompt is not exist")
	}
	return val, nil
}
