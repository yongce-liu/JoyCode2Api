package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/joycode"
)

var (
	chatModel     string
	chatStream    bool
	chatMaxTokens int
)

var chatCmd = &cobra.Command{
	Use:     "chat [message]",
	Short:   "发送聊天消息",
	Long:    "通过 JoyCode API 发送一条聊天消息并返回响应。",
	GroupID: "core",
	Example: `  # 发送简单消息
  joycode-proxy chat "你好"

  # 指定模型
  joycode-proxy chat -m GLM-5.1 "写一个排序算法"

  # 流式输出
  joycode-proxy chat -s "解释量子计算"`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := resolveClient()
		if err != nil {
			return err
		}
		body := map[string]interface{}{
			"model":      chatModel,
			"messages":   []map[string]interface{}{{"role": "user", "content": args[0]}},
			"stream":     false,
			"max_tokens": chatMaxTokens,
		}
		if chatStream {
			body["stream"] = true
			return streamChat(client, body)
		}
		payload, err := json.Marshal(body)
		if err != nil {
			return err
		}
		resp, err := client.Forward(joycode.EndpointChatCompletions, joycode.ProtocolOpenAI, payload)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		var result map[string]interface{}
		if err := json.Unmarshal(data, &result); err != nil {
			return fmt.Errorf("上游返回了非 JSON 响应 (HTTP %d): %s", resp.StatusCode, string(data))
		}
		choices, _ := result["choices"].([]interface{})
		if len(choices) > 0 {
			choice, _ := choices[0].(map[string]interface{})
			msg, _ := choice["message"].(map[string]interface{})
			content, _ := msg["content"].(string)
			fmt.Println(content)
		}
		return nil
	},
}

func streamChat(client *joycode.Client, body map[string]interface{}) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	resp, err := client.Forward(joycode.EndpointChatCompletions, joycode.ProtocolOpenAI, payload)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	buf := make([]byte, 4096)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			fmt.Print(string(buf[:n]))
		}
		if readErr != nil {
			break
		}
	}
	fmt.Println()
	return nil
}

func init() {
	chatCmd.Flags().StringVarP(&chatModel, "model", "m", "JoyAI-Code", "模型名称")
	chatCmd.Flags().BoolVarP(&chatStream, "stream", "s", false, "流式输出")
	chatCmd.Flags().IntVar(&chatMaxTokens, "max-tokens", 64000, "最大输出 token 数")
	rootCmd.AddCommand(chatCmd)
}
