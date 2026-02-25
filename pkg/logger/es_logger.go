package logger

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/elastic/go-elasticsearch/v7"
	"github.com/elastic/go-elasticsearch/v7/esapi"
)

type WrapperStruct struct {
	LogType   string      `json:"LOGTYPE"`
	Timestamp time.Time   `json:"@timestamp"`
	Data      interface{} `json:"data"`
}

func SendWrappedLog(client *elasticsearch.Client, streamName string, logType string, rawData interface{}) error {
	if client == nil {
		return nil
	}

	payload := WrapperStruct{
		LogType:   logType,
		Timestamp: time.Now(),
		Data:      rawData,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("JSON 序列化失败: %w", err)
	}

	var buf bytes.Buffer
	meta := fmt.Sprintf(`{"index":{"_index":"%s"}}`, streamName)
	buf.WriteString(meta)
	buf.WriteByte('\n')
	buf.Write(body)
	buf.WriteByte('\n')

	req := esapi.BulkRequest{
		Body: &buf,
	}

	res, err := req.Do(context.Background(), client)
	if err != nil {
		return fmt.Errorf("请求发送失败: %w", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		return fmt.Errorf("写入失败: %s", res.String())
	}

	// 检查 Bulk 响应中的 item 级错误
	var bulkResp struct {
		Errors bool `json:"errors"`
		Items  []struct {
			Index struct {
				Error  json.RawMessage `json:"error,omitempty"`
				Status int             `json:"status"`
			} `json:"index"`
		} `json:"items"`
	}
	if err := json.NewDecoder(res.Body).Decode(&bulkResp); err != nil {
		// 解码失败不阻断，只记录
		Warnf("[SendWrappedLog] 解析 bulk 响应失败: %v", err)
		return nil
	}
	if bulkResp.Errors {
		for _, item := range bulkResp.Items {
			if item.Index.Error != nil {
				return fmt.Errorf("bulk item 写入失败 (status=%d): %s", item.Index.Status, string(item.Index.Error))
			}
		}
	}

	return nil
}
